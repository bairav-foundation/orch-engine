// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

// `bairav dashboard`: a local, read-only web console.
//
// It is just another view of the same data `bairav ps` reads: K8s through
// the user's kubeconfig, docker through container labels. No server to
// deploy, no login — it can see exactly what your kubeconfig can see, and
// it binds to 127.0.0.1 unless told otherwise.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bairav-foundation/orch-engine/cli/internal/compose"
	"github.com/bairav-foundation/orch-engine/cli/internal/dashboard"
	"github.com/bairav-foundation/orch-engine/cli/internal/dlp"
	"github.com/bairav-foundation/orch-engine/cli/internal/k8sclient"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

var dashboardCmd = &cobra.Command{
	Use:     "dashboard",
	Aliases: []string{"ui"},
	Short:   "Open a local, read-only web console for the Orch Engine graph",
	Long: `Serve a web console on 127.0.0.1 showing Models, Skills, Agents, DLP
policies, AccessGrants and live DLP decisions, across the k8s and docker
targets. It refreshes every 3 seconds and changes nothing.

  bairav dashboard                    # http://127.0.0.1:7777
  bairav dashboard --demo             # scripted walkthrough, no cluster needed
  bairav dashboard --export-demo out  # write the demo as one static HTML file`,
	RunE: runDashboard,
}

func init() {
	dashboardCmd.Flags().String("listen", "127.0.0.1:7777", "address to serve on (keep it on loopback unless you know why)")
	dashboardCmd.Flags().Bool("demo", false, "serve the scripted demo instead of live data")
	dashboardCmd.Flags().String("export-demo", "", "write the demo as a self-contained index.html into this directory and exit")
	dashboardCmd.Flags().Bool("no-k8s", false, "skip Kubernetes (docker only)")
	dashboardCmd.Flags().Bool("no-docker", false, "skip docker (Kubernetes only)")
	rootCmd.AddCommand(dashboardCmd)
}

func runDashboard(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	if dir, _ := cmd.Flags().GetString("export-demo"); dir != "" {
		path, err := dashboard.Export(dir)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ Demo written to %s (one file, no external assets; host it anywhere)\n", path)
		return nil
	}

	listen, _ := cmd.Flags().GetString("listen")
	demo, _ := cmd.Flags().GetBool("demo")
	noK8s, _ := cmd.Flags().GetBool("no-k8s")
	noDocker, _ := cmd.Flags().GetBool("no-docker")

	h, err := dashboard.Handler(dashboard.Options{
		Demo: demo,
		Snapshot: func(ctx context.Context) dashboard.Snapshot {
			return buildSnapshot(ctx, !noK8s, !noDocker)
		},
	})
	if err != nil {
		return err
	}

	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", listen, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		fmt.Fprintf(cmd.ErrOrStderr(), "⚠ listening on %s: anyone who can reach it sees what your kubeconfig sees (read-only).\n", listen)
	} else {
		h = dashboard.LoopbackOnly(h) // DNS-rebinding guard
	}

	srv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		shut, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(shut)
	}()

	mode := "live"
	if demo {
		mode = "demo"
	}
	fmt.Fprintf(out, "bairav dashboard %s (%s, read-only)\n  open: http://%s\n  Ctrl-C to stop\n", version, mode, listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("dashboard: %w", err)
	}
	return nil
}

// buildSnapshot collects everything the dashboard shows. Every source is
// best-effort: a missing cluster or docker daemon becomes a warning in the
// UI, never an error page.
func buildSnapshot(ctx context.Context, withK8s, withDocker bool) dashboard.Snapshot {
	s := dashboard.Snapshot{
		Version:     version,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Mode:        "live",
		Models:      []dashboard.Node{},
		Skills:      []dashboard.Node{},
		Agents:      []dashboard.AgentNode{},
		DLPPolicies: []dashboard.PolicyNode{},
		Grants:      []dashboard.GrantNode{},
		Audit:       []dashboard.AuditEvent{},
	}
	collectLocalInto(&s)
	if withK8s {
		collectK8sInto(ctx, &s)
	}
	if withDocker {
		collectDockerInto(ctx, &s)
	}
	return s
}

func collectK8sInto(ctx context.Context, s *dashboard.Snapshot) {
	c, err := k8sclient.New()
	if err != nil {
		s.Warnings = append(s.Warnings, "kubernetes: "+err.Error())
		return
	}
	s.Context = k8sclient.CurrentContext()
	warn := func(what string, err error) {
		s.Warnings = append(s.Warnings, fmt.Sprintf("kubernetes: list %s: %v", what, err))
	}

	var models orchv1alpha1.ModelList
	if err := c.List(ctx, &models); err != nil {
		warn("models", err)
		return // cluster unreachable or CRDs missing; the rest will fail the same way
	}
	for _, m := range models.Items {
		s.Models = append(s.Models, dashboard.Node{
			Target: "k8s", Namespace: m.Namespace, Name: m.Name, Source: refOf(m.Spec).label(),
			Phase: phaseOr(string(m.Status.Phase), "Pending"), Endpoint: m.Status.Endpoint,
		})
	}

	var skills orchv1alpha1.SkillList
	if err := c.List(ctx, &skills); err != nil {
		warn("skills", err)
	}
	for _, sk := range skills.Items {
		s.Skills = append(s.Skills, dashboard.Node{
			Target: "k8s", Namespace: sk.Namespace, Name: sk.Name, Source: sk.Spec.Image,
			Phase: phaseOr(string(sk.Status.Phase), "Pending"), Endpoint: sk.Status.Endpoint,
			Transport: string(sk.Spec.Transport),
		})
	}

	var agents orchv1alpha1.AgentList
	if err := c.List(ctx, &agents); err != nil {
		warn("agents", err)
	}
	var dlpAgents []orchv1alpha1.Agent
	for _, a := range agents.Items {
		n := dashboard.AgentNode{
			Target: "k8s", Namespace: a.Namespace, Name: a.Name, Image: a.Spec.Image,
			Phase:       phaseOr(string(a.Status.Phase), "Pending"),
			Models:      refsToStrings(a.Namespace, a.Spec.ModelRefs),
			Skills:      refsToStrings(a.Namespace, a.Spec.SkillRefs),
			BoundModels: nonNil(a.Status.BoundModels),
			BoundSkills: nonNil(a.Status.BoundSkills),
			Message:     agentMessage(a),
			Endpoint:    a.Status.Endpoint,
		}
		if a.Spec.DLPRef != nil {
			n.DLPPolicy = a.Spec.DLPRef.Name
			dlpAgents = append(dlpAgents, a)
		}
		s.Agents = append(s.Agents, n)
	}

	var pols orchv1alpha1.DLPPolicyList
	if err := c.List(ctx, &pols); err != nil {
		warn("dlppolicies", err)
	}
	for _, p := range pols.Items {
		pn := dashboard.PolicyNode{
			Namespace: p.Namespace, Name: p.Name, Phase: phaseOr(string(p.Status.Phase), "Pending"),
			Direction: string(p.Spec.Direction), Action: string(p.Spec.DefaultAction),
			Packs: nonNil(p.Spec.Packs), Rules: p.Status.RuleCount,
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == "Validated" && cond.Status == metav1.ConditionFalse {
				pn.Message = cond.Message
			}
		}
		s.DLPPolicies = append(s.DLPPolicies, pn)
	}

	var grants orchv1alpha1.AccessGrantList
	if err := c.List(ctx, &grants); err != nil {
		warn("accessgrants", err)
	}
	for _, g := range grants.Items {
		gn := dashboard.GrantNode{Namespace: g.Namespace, Name: g.Name, From: []string{}, To: []string{}}
		for _, f := range g.Spec.From {
			gn.From = append(gn.From, f.Namespace)
		}
		for _, t := range g.Spec.To {
			gn.To = append(gn.To, describeTarget(t))
		}
		s.Grants = append(s.Grants, gn)
	}

	if len(dlpAgents) > 0 {
		s.Audit = append(s.Audit, collectAudit(ctx, c, dlpAgents, s)...)
	}
}

// collectAudit tails the DLP sidecars' stdout (JSONL audit) for
// Agents with a dlpRef. Bounded: at most 20 Agents, 30 lines per sidecar,
// 60 events total — this runs on every 3-second refresh.
func collectAudit(ctx context.Context, c ctrlclient.Client, agents []orchv1alpha1.Agent, s *dashboard.Snapshot) []dashboard.AuditEvent {
	cs, err := k8sclient.NewClientset()
	if err != nil {
		s.Warnings = append(s.Warnings, "dlp audit: "+err.Error())
		return nil
	}
	if len(agents) > 20 {
		agents = agents[:20]
	}
	var events []dashboard.AuditEvent
	for _, a := range agents {
		var pods corev1.PodList
		if err := c.List(ctx, &pods, ctrlclient.InNamespace(a.Namespace),
			ctrlclient.MatchingLabels{"orch.bairavs.com/agent": a.Name}); err != nil {
			continue
		}
		for _, p := range pods.Items {
			if p.Status.Phase != corev1.PodRunning {
				continue
			}
			for _, ct := range p.Spec.Containers {
				if !strings.HasPrefix(ct.Name, "dlp-") {
					continue
				}
				events = append(events, tailAudit(ctx, cs, a, p.Name, ct.Name)...)
			}
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time > events[j].Time })
	if len(events) > 60 {
		events = events[:60]
	}
	return events
}

func tailAudit(ctx context.Context, cs kubernetes.Interface, a orchv1alpha1.Agent, pod, container string) []dashboard.AuditEvent {
	tail := int64(30)
	rc, err := cs.CoreV1().Pods(a.Namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, TailLines: &tail}).Stream(ctx)
	if err != nil {
		return nil
	}
	defer rc.Close()
	var out []dashboard.AuditEvent
	sc := bufio.NewScanner(rc)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue // banner / non-audit lines
		}
		var e dlp.AuditEntry
		if json.Unmarshal(line, &e) != nil || e.Timestamp.IsZero() {
			continue
		}
		out = append(out, dashboard.AuditEvent{
			Time: e.Timestamp.UTC().Format(time.RFC3339), Namespace: a.Namespace, Agent: a.Name,
			Model: strings.TrimPrefix(container, "dlp-"), Direction: string(e.Direction),
			Action: string(e.Action), Labels: nonNil(e.Labels), ResponseLabels: e.ResponseLabels,
			LatencyMS: e.LatencyMS,
		})
	}
	return out
}

func collectDockerInto(ctx context.Context, s *dashboard.Snapshot) {
	stacks, err := listDocker(ctx)
	if err != nil {
		s.Warnings = append(s.Warnings, "docker: "+err.Error())
		return
	}
	seenModel := map[string]bool{}
	for _, st := range stacks {
		phase := dockerPhase(st.Status)
		switch {
		case st.Kind == compose.KindModel:
			// Enforce-mode models are two containers; show one node, and
			// prefer the door (the container with a network endpoint).
			if seenModel[st.Name] && st.NetEndpoint == "" {
				continue
			}
			if seenModel[st.Name] {
				for i := range s.Models {
					if s.Models[i].Target == "docker" && s.Models[i].Name == st.Name {
						s.Models[i].Endpoint = st.Endpoint
					}
				}
				continue
			}
			seenModel[st.Name] = true
			s.Models = append(s.Models, dashboard.Node{Target: "docker", Namespace: "docker", Name: st.Name,
				Source: dockerSource(st), Phase: phase, Endpoint: st.Endpoint})
		case st.Kind == compose.KindSkill:
			s.Skills = append(s.Skills, dashboard.Node{Target: "docker", Namespace: "docker", Name: st.Name,
				Source: st.Image, Phase: phase, Endpoint: st.Endpoint, Transport: st.Labels[compose.LabelTransport]})
		case st.Kind == compose.KindAgent && st.Role == compose.RoleAgent:
			n := dashboard.AgentNode{Target: "docker", Namespace: "docker", Name: st.Name, Image: st.Image,
				Phase: phase, Models: []string{}, Skills: []string{}, Endpoint: st.Endpoint}
			for k := range st.Labels {
				if m := strings.TrimPrefix(k, compose.LabelRefModelPrefix); m != k {
					n.Models = append(n.Models, m)
				}
				if sk := strings.TrimPrefix(k, compose.LabelRefSkillPrefix); sk != k {
					n.Skills = append(n.Skills, sk)
				}
			}
			sort.Strings(n.Models)
			sort.Strings(n.Skills)
			// Refs resolved at deploy time; a running agent is wired to all of them.
			if phase == "Ready" {
				n.BoundModels, n.BoundSkills = n.Models, n.Skills
			} else {
				n.BoundModels, n.BoundSkills = []string{}, []string{}
			}
			s.Agents = append(s.Agents, n)
		}
	}
}

// collectLocalInto adds bare-process models started with `bairav model
// serve` (the LOCAL section of `bairav ps`). Note `bairav model list`
// shows the download cache, not running models, so a cached-but-not-
// served model correctly does NOT appear on the graph.
func collectLocalInto(s *dashboard.Snapshot) {
	rows, err := collectLocal()
	if err != nil {
		s.Warnings = append(s.Warnings, "local: "+err.Error())
		return
	}
	for _, r := range rows {
		phase := "Ready"
		if r.health != "ok" {
			phase = "Deploying"
		}
		s.Models = append(s.Models, dashboard.Node{Target: "local", Namespace: "local",
			Name: fmt.Sprintf("%s:%d", lastSegment(r.state.Model), r.state.Port), Source: r.state.Model,
			Phase: phase, Endpoint: r.state.Endpoint()})
	}
}

// dockerPhase maps `docker ps` status text onto the dashboard's phases.
func dockerPhase(status string) string {
	st := strings.ToLower(status)
	switch {
	case strings.Contains(st, "unhealthy"):
		return "Failed"
	case strings.Contains(st, "health: starting"):
		return "Deploying"
	case strings.HasPrefix(st, "up"):
		return "Ready"
	case strings.HasPrefix(st, "restarting"):
		return "Deploying"
	default:
		return "Failed" // exited / dead / created
	}
}

// agentMessage surfaces the most useful condition text: Degraded first,
// then Progressing while not Ready.
func agentMessage(a orchv1alpha1.Agent) string {
	var progressing string
	for _, cond := range a.Status.Conditions {
		if cond.Type == "Degraded" && cond.Status == metav1.ConditionTrue {
			return cond.Message
		}
		if cond.Type == "Progressing" && cond.Status == metav1.ConditionTrue {
			progressing = cond.Message
		}
	}
	if a.Status.Phase == orchv1alpha1.AgentPhaseReady {
		return ""
	}
	return progressing
}

func refsToStrings(agentNS string, refs []orchv1alpha1.LocalObjectReference) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, qualifiedRef(agentNS, r.Namespace, r.Name))
	}
	return out
}

// nonNil keeps JSON arrays as [] rather than null, which keeps the UI code
// free of null checks.
func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// dockerSource shows a cloud stack as "provider:model".
func dockerSource(st compose.Stack) string {
	if p := st.Labels[compose.LabelProvider]; p != "" {
		return p + ":" + st.Repo
	}
	return st.Repo
}
