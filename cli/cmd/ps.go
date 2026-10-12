// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bairav-foundation/orch-engine/cli/internal/compose"
	"github.com/bairav-foundation/orch-engine/cli/internal/k8sclient"
	"github.com/bairav-foundation/orch-engine/cli/internal/serve"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// psCmd lists the running graph across every target:
//   - bare-process Models (via ~/.bairav/run/ state files)
//   - Docker Compose stacks (via `docker ps` labels)
//   - Model + Skill + Agent CRs in the active K8s context
//
// Extended to include Skill and Agent CRs alongside Models —
// the KUBERNETES section is now three sub-tables (MODELS / SKILLS /
// AGENTS) so an operator sees the whole graph in one command.
//
// Local source of truth: state files at ~/.bairav/run/port-<N>.json, written
// by `bairav model serve` on start and removed on clean shutdown. We probe
// each endpoint + check PID liveness and prune stale entries automatically.
//
// Docker source of truth: `docker ps` filtered to label bairav.managed=true.
// Silently skipped when docker isn't installed or its daemon is down.
//
// K8s source of truth: Model CRs across all namespaces in the active context.
// Silently skipped when no kubeconfig is reachable.
var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "List BAIRAVS-served models (local + docker + kubernetes)",
	Long: `Show every model server currently running across all three deploy
targets: bare-process (LOCAL), Docker Compose (DOCKER), Model CRs in the
active kubeconfig context (KUBERNETES).

Sections render only when there's something to show. If nothing is running
anywhere, prints a hint with how to start one.`,
	RunE: runPS,
}

func init() {
	psCmd.Flags().Bool("local-only", false,
		"skip the DOCKER and KUBERNETES sections")
	psCmd.Flags().Bool("no-docker", false,
		"skip the DOCKER section")
	psCmd.Flags().Bool("no-k8s", false,
		"skip the KUBERNETES section")
	psCmd.Flags().String("k8s-namespace", "",
		"limit the KUBERNETES section to one namespace (default: all)")
}

func runPS(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	localOnly, _ := cmd.Flags().GetBool("local-only")
	noDocker, _ := cmd.Flags().GetBool("no-docker")
	noK8s, _ := cmd.Flags().GetBool("no-k8s")
	k8sNS, _ := cmd.Flags().GetString("k8s-namespace")

	if localOnly {
		noDocker, noK8s = true, true
	}

	wroteSomething := false

	// --- LOCAL section --------------------------------------------------
	live, err := collectLocal()
	if err != nil {
		return err
	}
	if len(live) > 0 {
		fmt.Fprintln(out, "LOCAL")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  PORT\tMODEL\tBACKEND\tPID\tUPTIME\tHEALTH\tENDPOINT")
		for _, r := range live {
			uptime := time.Since(r.state.StartedAt).Round(time.Second).String()
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%d\t%s\t%s\t%s\n",
				r.state.Port, r.state.Model, r.state.Backend,
				r.state.PID, uptime, r.health, r.state.Endpoint())
		}
		_ = tw.Flush()
		wroteSomething = true
	}

	// --- DOCKER section -------------------------------------------------
	if !noDocker {
		if stacks, err := collectDocker(); err == nil && len(stacks) > 0 {
			if wroteSomething {
				fmt.Fprintln(out)
			}
			fmt.Fprintln(out, "DOCKER  (compose)")
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			// KIND column (model / skill / agent, with "/dlp" for a
			// stack's DLP proxy container) and SOURCE = HF repo for models,
			// image for skills + agents.
			fmt.Fprintln(tw, "  KIND\tNAME\tSOURCE\tSTATUS\tENDPOINT")
			for _, s := range stacks {
				kind := s.Kind
				if s.Role == compose.RoleDLP {
					kind += "/dlp"
				}
				source := s.Repo
				if source == "" {
					source = s.Image
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
					kind, s.Name, or(source, "—"), s.Status, or(s.Endpoint, "—"))
			}
			_ = tw.Flush()
			wroteSomething = true
		}
	}

	// --- KUBERNETES section --------------------------------------------
	// Renders three sub-tables under one KUBERNETES banner: MODELS,
	// SKILLS, AGENTS. Sub-tables with zero rows are skipped, so a
	// Model-only cluster still reads cleanly. All three queries share
	// one k8sclient.New() call inside collectK8sGraph.
	if !noK8s {
		graph, ctxName, err := collectK8sGraph(k8sNS)
		if err == nil && graph.total() > 0 {
			if wroteSomething {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "KUBERNETES  (context: %s)\n", ctxName)
			renderModelRows(out, graph.models)
			renderSkillRows(out, graph.skills)
			renderAgentRows(out, graph.agents)
			wroteSomething = true
		}
	}

	if !wroteSomething {
		fmt.Fprintln(out, "Nothing running.")
		fmt.Fprintln(out, "Start something:")
		fmt.Fprintln(out, "  bairav model serve <hf-repo>                          # bare process on this machine")
		fmt.Fprintln(out, "  bairav model deploy <hf-repo>                         # docker compose (default)")
		fmt.Fprintln(out, "  bairav model deploy <hf-repo> --target k8s            # active kubeconfig context")
		fmt.Fprintln(out, "  bairav skill deploy <image>                           # MCP server (docker default; --target k8s for the cluster)")
		fmt.Fprintln(out, "  bairav agent deploy <image> --model X --skill Y       # user's agent code, wired to model + skills")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Or see it all in a browser: bairav dashboard   (try --demo with no cluster)")
	}
	return nil
}

// collectDocker fetches running bairav compose stacks. Best-effort: docker
// not installed / daemon down returns (nil, nil) so the section just vanishes.
func collectDocker() ([]compose.Stack, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return compose.List(ctx)
}

// localRow is one row of the LOCAL section.
type localRow struct {
	state  serve.State
	health string
}

// k8sRow is one row of the KUBERNETES section.
type k8sRow struct {
	namespace string
	name      string
	repo      string
	phase     string
	endpoint  string
}

// collectLocal reads + prunes the run-state files into renderable rows.
func collectLocal() ([]localRow, error) {
	states, err := serve.ReadAllStates()
	if err != nil {
		return nil, fmt.Errorf("read run dir: %w", err)
	}
	var out []localRow
	for _, s := range states {
		alive := serve.PIDAlive(s.PID)
		healthy := probeOpenAIEndpoint(s.Endpoint())
		if !alive && !healthy {
			_ = serve.RemoveState(s.Port)
			continue
		}
		health := "ok"
		switch {
		case healthy && alive:
			health = "ok"
		case alive && !healthy:
			health = "loading…"
		case !alive && healthy:
			health = "ok (pid stale)"
		}
		out = append(out, localRow{state: s, health: health})
	}
	return out, nil
}

// k8sGraph is the full triple of what `bairav ps` renders under
// KUBERNETES: Models + Skills + Agents. Held together in one struct so
// the rendering loop can decide "did we find anything at all" without
// interrogating each list separately.
type k8sGraph struct {
	models []k8sRow
	skills []skillRow
	agents []agentRow
}

func (g k8sGraph) total() int { return len(g.models) + len(g.skills) + len(g.agents) }

// skillRow is one row of the KUBERNETES → SKILLS sub-table.
type skillRow struct {
	namespace string
	name      string
	image     string
	phase     string
	endpoint  string
}

// agentRow is one row of the KUBERNETES → AGENTS sub-table.
// Includes the bound Model/Skill names so operators can eyeball the
// graph without switching to `kubectl describe agent`.
//
// declaredModels / declaredSkills carry the Spec-side refs (what the
// user declared), while boundModels / boundSkills come from Status
// (what actually resolved). `bairav ps` renders the Status view;
// `bairav visualize` uses the Spec view so unresolved edges still draw.
type agentRow struct {
	namespace      string
	name           string
	image          string
	phase          string
	declaredModels []string
	declaredSkills []string
	boundModels    string
	boundSkills    string
	endpoint       string
}

// collectK8sGraph queries Model + Skill + Agent CRs in one go, sharing
// a single controller-runtime client. Returns ("", empty, err) silently
// when no kubeconfig is reachable — that's the laptop-only happy path
// and shouldn't produce user-visible noise.
func collectK8sGraph(k8sNS string) (k8sGraph, string, error) {
	c, err := k8sclient.New()
	if err != nil {
		return k8sGraph{}, "", err
	}
	ctxName := k8sclient.CurrentContext()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := []ctrlclient.ListOption{}
	if k8sNS != "" {
		opts = append(opts, ctrlclient.InNamespace(k8sNS))
	}

	var mList orchv1alpha1.ModelList
	if err := c.List(ctx, &mList, opts...); err != nil {
		return k8sGraph{}, ctxName, err
	}
	var sList orchv1alpha1.SkillList
	if err := c.List(ctx, &sList, opts...); err != nil {
		return k8sGraph{}, ctxName, err
	}
	var aList orchv1alpha1.AgentList
	if err := c.List(ctx, &aList, opts...); err != nil {
		return k8sGraph{}, ctxName, err
	}

	g := k8sGraph{
		models: make([]k8sRow, 0, len(mList.Items)),
		skills: make([]skillRow, 0, len(sList.Items)),
		agents: make([]agentRow, 0, len(aList.Items)),
	}
	for _, m := range mList.Items {
		g.models = append(g.models, k8sRow{
			namespace: m.Namespace,
			name:      m.Name,
			repo:      m.Spec.Repo,
			phase:     phaseOr(string(m.Status.Phase), "Pending"),
			endpoint:  or(m.Status.Endpoint, "—"),
		})
	}
	for _, s := range sList.Items {
		g.skills = append(g.skills, skillRow{
			namespace: s.Namespace,
			name:      s.Name,
			image:     s.Spec.Image,
			phase:     phaseOr(string(s.Status.Phase), "Pending"),
			endpoint:  or(s.Status.Endpoint, "—"),
		})
	}
	for _, a := range aList.Items {
		declModels := make([]string, 0, len(a.Spec.ModelRefs))
		for _, r := range a.Spec.ModelRefs {
			declModels = append(declModels, qualifiedRef(a.Namespace, r.Namespace, r.Name))
		}
		declSkills := make([]string, 0, len(a.Spec.SkillRefs))
		for _, r := range a.Spec.SkillRefs {
			declSkills = append(declSkills, qualifiedRef(a.Namespace, r.Namespace, r.Name))
		}
		g.agents = append(g.agents, agentRow{
			namespace:      a.Namespace,
			name:           a.Name,
			image:          a.Spec.Image,
			phase:          phaseOr(string(a.Status.Phase), "Pending"),
			declaredModels: declModels,
			declaredSkills: declSkills,
			boundModels:    or(joinNonEmpty(a.Status.BoundModels), "—"),
			boundSkills:    or(joinNonEmpty(a.Status.BoundSkills), "—"),
			endpoint:       or(a.Status.Endpoint, "—"),
		})
	}
	return g, ctxName, nil
}

// renderModelRows / renderSkillRows / renderAgentRows print a sub-table
// under the KUBERNETES banner. Each is a no-op when its row slice is
// empty so a Model-only cluster still reads cleanly.
func renderModelRows(out io.Writer, rows []k8sRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(out, "  MODELS")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    NAMESPACE\tNAME\tREPO\tPHASE\tENDPOINT")
	for _, r := range rows {
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\n",
			r.namespace, r.name, r.repo, r.phase, r.endpoint)
	}
	_ = tw.Flush()
}

func renderSkillRows(out io.Writer, rows []skillRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  SKILLS")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    NAMESPACE\tNAME\tIMAGE\tPHASE\tENDPOINT")
	for _, r := range rows {
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\n",
			r.namespace, r.name, r.image, r.phase, r.endpoint)
	}
	_ = tw.Flush()
}

func renderAgentRows(out io.Writer, rows []agentRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  AGENTS")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    NAMESPACE\tNAME\tIMAGE\tPHASE\tMODELS\tSKILLS\tENDPOINT")
	for _, r := range rows {
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.namespace, r.name, r.image, r.phase, r.boundModels, r.boundSkills, r.endpoint)
	}
	_ = tw.Flush()
}

// qualifiedRef renders a ref the way AgentStatus.BoundModels does: bare
// name when it's in the Agent's namespace, "ns/name" when cross-namespace.
// Keeping both in one format lets visualize compare them.
func qualifiedRef(agentNS, refNS, name string) string {
	if refNS == "" || refNS == agentNS {
		return name
	}
	return refNS + "/" + name
}

// phaseOr / or / joinNonEmpty are tiny formatting helpers. Kept local
// to ps.go — they're not general enough for a shared package.
func phaseOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
func joinNonEmpty(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return strings.Join(ss, ",")
}

// probeOpenAIEndpoint does a 1-second GET on /models. Returns true iff the
// server responds with HTTP 200.
func probeOpenAIEndpoint(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
