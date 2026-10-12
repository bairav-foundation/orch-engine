// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bairav-foundation/orch-engine/cli/internal/compose"
	"github.com/bairav-foundation/orch-engine/cli/internal/k8sclient"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// agentCmd groups `bairav agent <subcommand>` — deploy, undeploy, list.
//
// An Agent is a user-provided container that runs their agent framework
// code (LangGraph, AutoGen, Anthropic SDK, custom Go — whatever). The
// Agent CR declares which Models and Skills the container needs; the
// operator resolves them, waits for Ready, and injects endpoint env
// vars before rolling the Pod. **We do not write the agent.**
//
// This is the strategic climax the venture pitch has been aiming at:
// Model + Skill + DLP + user code, all under one CRD group,
// declaratively deployed as a graph.
var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage Agents (user-provided agent containers that reference Models + Skills)",
	Long: `Deploy the user's agent framework code as an Agent CR. The operator
resolves every ModelRef and SkillRef in the same namespace, waits for
each dependency to be Ready, injects endpoint env vars, and rolls the
Pod. The Agent's container ends up with:

  BAIRAV_MODEL_<NAME>_ENDPOINT   for each referenced Model
  BAIRAV_SKILL_<NAME>_ENDPOINT   for each referenced Skill
  BAIRAV_SKILL_<NAME>_TRANSPORT  ("sse" | "http") for each Skill

The user's code reads those env vars and does whatever agentic work it
wants. BAIRAVS provides the runtime substrate; the intelligence stays
in the user's container.`,
}

func init() {
	agentCmd.AddCommand(agentDeployCmd)
	agentCmd.AddCommand(agentUndeployCmd)
	agentCmd.AddCommand(agentListCmd)

	agentDeployCmd.Flags().StringP("name", "n", "",
		"name of the Agent CR (default: derived from <image>)")
	agentDeployCmd.Flags().String("namespace", "default",
		"Kubernetes namespace to deploy into")
	agentDeployCmd.Flags().StringSlice("model", nil,
		"Model to reference (repeatable). <name> = same namespace; <ns>/<name> = another namespace (k8s, needs `bairav grant` there)")
	agentDeployCmd.Flags().StringSlice("skill", nil,
		"Skill to reference (repeatable). <name> = same namespace; <ns>/<name> = another namespace (k8s, needs `bairav grant` there)")
	agentDeployCmd.Flags().String("dlp-policy", "",
		"k8s target: DLPPolicy CR (same namespace) to enforce on this Agent's Model traffic via a DLP sidecar")
	agentDeployCmd.Flags().String("target", "",
		"where to deploy: docker (default) | k8s")
	agentDeployCmd.Flags().Int("host-port", 0,
		"docker target: host port to publish the agent on (0 = docker picks a free port)")
	// Docker-target DLP. There is no DLPPolicy CR on docker, so the policy
	// is given inline as flags. Setting any --dlp-* flag implies --dlp.
	agentDeployCmd.Flags().Bool("dlp", false,
		"docker target: put a DLP proxy in front of every Model the agent calls")
	agentDeployCmd.Flags().StringSlice("dlp-pack", nil,
		"docker target: DLP packs (e.g. global-safe,india); default none = built-ins only")
	agentDeployCmd.Flags().String("dlp-action", "",
		"docker target: redact | block | audit (default redact)")
	agentDeployCmd.Flags().String("dlp-direction", "",
		"docker target: request | response | both (default both)")
	agentDeployCmd.Flags().String("dlp-policy-file", "",
		"docker target: YAML policy file with custom rules, mounted read-only")
	agentDeployCmd.Flags().Int("port", 8000,
		"port the agent's HTTP endpoint listens on inside the container")
	agentDeployCmd.Flags().String("health-path", "/health",
		"HTTP path used by readiness/liveness probes")
	agentDeployCmd.Flags().Bool("wait", true,
		"poll until phase=Ready before returning")
	agentDeployCmd.Flags().Duration("timeout", 5*time.Minute,
		"max time to wait for phase=Ready when --wait is set (longer than Skill because deps must resolve first)")

	agentUndeployCmd.Flags().String("target", "",
		"where the Agent runs: docker (default) | k8s")
	agentUndeployCmd.Flags().String("namespace", "default",
		"namespace the Agent lives in")
	agentUndeployCmd.Flags().BoolP("force", "f", false,
		"skip the confirmation prompt")

	agentListCmd.Flags().String("target", "", "where to list: docker (default) | k8s")
	agentListCmd.Flags().String("namespace", "default", "namespace to list from")
	agentListCmd.Flags().BoolP("all-namespaces", "A", false, "list across every namespace")
}

// -------------------------------------------------------------------------
// agent deploy
// -------------------------------------------------------------------------

var agentDeployCmd = &cobra.Command{
	Use:   "deploy <image>",
	Short: "Deploy an Agent referencing Models + Skills (docker by default, --target k8s for an Agent CR)",
	Long: `Deploy a user-provided agent container that references existing
Models and Skills by name (same namespace). The operator waits for
every reference to be Ready before rolling the Agent Pod.

Example:
  bairav agent deploy ghcr.io/myorg/support-agent:v1 --target k8s \
    --model qwen2-5-0-5b \
    --skill filesystem \
    --skill customer-lookup

  bairav agent deploy ghcr.io/myorg/tool-router:v2 --skill filesystem --skill web-search --port 9000 --target k8s

  # Docker target (no cluster): refs resolve against running docker stacks
  bairav agent deploy ghcr.io/myorg/support-agent:v1 --target docker \
    --model qwen2-5-0-5b --skill echo --dlp --dlp-pack global-safe`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentDeploy,
}

func runAgentDeploy(cmd *cobra.Command, args []string) error {
	image := args[0]
	name, _ := cmd.Flags().GetString("name")
	namespace, _ := cmd.Flags().GetString("namespace")
	models, _ := cmd.Flags().GetStringSlice("model")
	skills, _ := cmd.Flags().GetStringSlice("skill")
	port, _ := cmd.Flags().GetInt("port")
	healthPath, _ := cmd.Flags().GetString("health-path")
	wait, _ := cmd.Flags().GetBool("wait")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	dlpPolicy, _ := cmd.Flags().GetString("dlp-policy")
	dlpPolicy = strings.TrimSpace(dlpPolicy)

	if name == "" {
		name = skillNameFromImage(image) // same shape as Skill's naming — good enough for Agent too
	}
	out := cmd.OutOrStdout()

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	target, err := resolveCmdTarget(cmd)
	if err != nil {
		if dlpPolicy != "" && strings.Contains(err.Error(), "--dlp-policy ") {
			return fmt.Errorf("%w (--dlp-policy names a DLPPolicy CR; on docker use --dlp with --dlp-pack / --dlp-policy-file)", err)
		}
		return err
	}
	if target == "docker" {
		o := agentDockerOpts{name: name, image: image, port: port, models: models, skills: skills}
		o.hostPort, _ = cmd.Flags().GetInt("host-port")
		o.dlp, _ = cmd.Flags().GetBool("dlp")
		o.dlpPacks, _ = cmd.Flags().GetStringSlice("dlp-pack")
		o.dlpAction, _ = cmd.Flags().GetString("dlp-action")
		o.dlpDir, _ = cmd.Flags().GetString("dlp-direction")
		o.dlpPolicyFile, _ = cmd.Flags().GetString("dlp-policy-file")
		for _, f := range []string{"dlp-pack", "dlp-action", "dlp-direction", "dlp-policy-file"} {
			if cmd.Flags().Changed(f) {
				o.dlp = true
			}
		}
		return deployAgentToDocker(ctx, cmd, o)
	}

	c, err := k8sclient.New()
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Context:    %s\n", k8sclient.CurrentContext())
	fmt.Fprintf(out, "Namespace:  %s\n", namespace)
	fmt.Fprintf(out, "Name:       %s\n", name)
	fmt.Fprintf(out, "Image:      %s\n", image)
	fmt.Fprintf(out, "Port:       %d\n", port)
	if len(models) > 0 {
		fmt.Fprintf(out, "Models:     %s\n", strings.Join(models, ", "))
	}
	if len(skills) > 0 {
		fmt.Fprintf(out, "Skills:     %s\n", strings.Join(skills, ", "))
	}
	if dlpPolicy != "" {
		fmt.Fprintf(out, "DLP:        %s (sidecar per Model)\n", dlpPolicy)
	}
	fmt.Fprintln(out)

	if err := ensureNamespace(ctx, c, namespace, out); err != nil {
		return err
	}

	desired := buildAgentCR(name, namespace, image, healthPath, int32(port), models, skills)
	if dlpPolicy != "" {
		desired.Spec.DLPRef = &orchv1alpha1.LocalObjectReference{Name: dlpPolicy}
	}

	var existing orchv1alpha1.Agent
	key := types.NamespacedName{Name: name, Namespace: namespace}
	err = c.Get(ctx, key, &existing)
	switch {
	case apierrors.IsNotFound(err):
		fmt.Fprintf(out, "Creating Agent %s/%s ...\n", namespace, name)
		if err := c.Create(ctx, desired); err != nil {
			return fmt.Errorf("create agent: %w", err)
		}
		fmt.Fprintln(out, "✓ Created")
	case err == nil:
		fmt.Fprintf(out, "Updating existing Agent %s/%s ...\n", namespace, name)
		existing.Spec = desired.Spec
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update agent: %w", err)
		}
		fmt.Fprintln(out, "✓ Updated")
	default:
		return fmt.Errorf("get agent: %w", err)
	}

	if !wait {
		fmt.Fprintln(out, "(--wait=false; not polling for Ready)")
		return nil
	}
	return pollAgentUntilReady(ctx, c, key, timeout, out, cmd.ErrOrStderr())
}

// -------------------------------------------------------------------------
// agent undeploy
// -------------------------------------------------------------------------

var agentUndeployCmd = &cobra.Command{
	Use:   "undeploy <name>",
	Short: "Remove an Agent (docker stack by default, or an Agent CR with --target k8s)",
	Args:  cobra.ExactArgs(1),
	RunE:  runAgentUndeploy,
}

func runAgentUndeploy(cmd *cobra.Command, args []string) error {
	name := args[0]
	namespace, _ := cmd.Flags().GetString("namespace")
	force, _ := cmd.Flags().GetBool("force")
	out := cmd.OutOrStdout()

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	target, err := resolveCmdTarget(cmd)
	if err != nil {
		return err
	}
	if target == "docker" {
		return undeployAgentFromDocker(ctx, cmd, name, force)
	}

	c, err := k8sclient.New()
	if err != nil {
		return err
	}

	var a orchv1alpha1.Agent
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := c.Get(ctx, key, &a); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("agent %s/%s not found in current context", namespace, name)
		}
		return fmt.Errorf("get agent: %w", err)
	}

	fmt.Fprintf(out, "About to delete Agent %s/%s (image: %s, phase: %s).\n",
		namespace, name, a.Spec.Image, a.Status.Phase)
	if !force {
		fmt.Fprint(out, "Type 'yes' to confirm: ")
		var ans string
		_, _ = fmt.Fscanln(cmd.InOrStdin(), &ans)
		if strings.TrimSpace(strings.ToLower(ans)) != "yes" {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}
	if err := c.Delete(ctx, &a); err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	fmt.Fprintf(out, "✓ Deleted Agent %s/%s\n", namespace, name)
	return nil
}

// -------------------------------------------------------------------------
// agent list
// -------------------------------------------------------------------------

var agentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Agents (docker by default; --target k8s for Agent CRs)",
	RunE:  runAgentList,
}

func runAgentList(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	target, err := resolveCmdTarget(cmd)
	if err != nil {
		return err
	}
	if target == "docker" {
		return listDockerKind(cmd, compose.KindAgent)
	}
	namespace, _ := cmd.Flags().GetString("namespace")
	allNS, _ := cmd.Flags().GetBool("all-namespaces")

	c, err := k8sclient.New()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var list orchv1alpha1.AgentList
	opts := []ctrlclient.ListOption{}
	if !allNS {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}
	if err := c.List(ctx, &list, opts...); err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	if len(list.Items) == 0 {
		if allNS {
			fmt.Fprintln(out, "No Agents found in any namespace.")
		} else {
			fmt.Fprintf(out, "No Agents found in namespace %q.\n", namespace)
		}
		fmt.Fprintln(out, "Deploy one: bairav agent deploy <image> --model <name> --skill <name> --target k8s")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tNAME\tIMAGE\tPHASE\tMODELS\tSKILLS\tENDPOINT")
	for _, a := range list.Items {
		phase := string(a.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		endpoint := a.Status.Endpoint
		if endpoint == "" {
			endpoint = "—"
		}
		models := strings.Join(a.Status.BoundModels, ",")
		if models == "" {
			models = "—"
		}
		skills := strings.Join(a.Status.BoundSkills, ",")
		if skills == "" {
			skills = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.Namespace, a.Name, a.Spec.Image, phase, models, skills, endpoint)
	}
	_ = tw.Flush()
	return nil
}

// -------------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------------

// buildAgentCR turns CLI args into a v1alpha1 Agent spec.
func buildAgentCR(name, namespace, image, healthPath string, port int32, models, skills []string) *orchv1alpha1.Agent {
	return &orchv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "bairav",
			},
		},
		Spec: orchv1alpha1.AgentSpec{
			Image:      image,
			Port:       port,
			HealthPath: healthPath,
			ModelRefs:  refsFromNames(models),
			SkillRefs:  refsFromNames(skills),
		},
	}
}

// listDependentAgents returns every Agent in `namespace` whose refs
// point at (kind, name). kind must be "Model" or "Skill". Used by
// model + skill undeploy to warn about (or cascade-delete) Agents that
// would be orphaned. Errors on list failure so the caller can surface
// a clean "we didn't proceed because we couldn't check dependents"
// message rather than silently deleting.
func listDependentAgents(ctx context.Context, c ctrlclient.Client, kind, name, namespace string) ([]orchv1alpha1.Agent, error) {
	// Cluster-wide since Day 22: Agents in other namespaces can reference
	// this Model/Skill through an AccessGrant, and they must show up here
	// (and be cascaded) like local ones.
	var list orchv1alpha1.AgentList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	out := make([]orchv1alpha1.Agent, 0)
	for _, a := range list.Items {
		refs := a.Spec.ModelRefs
		if kind == "Skill" {
			refs = a.Spec.SkillRefs
		}
		matches := false
		for _, ref := range refs {
			refNS := ref.Namespace
			if refNS == "" {
				refNS = a.Namespace
			}
			if ref.Name == name && refNS == namespace {
				matches = true
				break
			}
		}
		if matches {
			out = append(out, a)
		}
	}
	return out, nil
}

// printDependentAgents shows the list of Agents about to be affected by
// a Model/Skill deletion and describes the outcome based on --cascade-agents.
// Called from both model undeploy and skill undeploy paths so the UX is
// identical: same wording, same list shape, no surprises.
func printDependentAgents(out io.Writer, kind string, dependents []orchv1alpha1.Agent, cascade bool) {
	if len(dependents) == 0 {
		return
	}
	fmt.Fprintf(out, "%d Agent(s) reference this %s:\n", len(dependents), kind)
	for _, a := range dependents {
		phase := string(a.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		fmt.Fprintf(out, "  - %s/%s (%s)\n", a.Namespace, a.Name, phase)
	}
	if cascade {
		fmt.Fprintln(out, "These Agents will ALSO be deleted (--cascade-agents).")
	} else {
		fmt.Fprintln(out, "These Agents will remain but move to WaitingForDeps.")
		fmt.Fprintln(out, "Pass --cascade-agents to delete them alongside the "+strings.ToLower(kind)+".")
	}
}

// cascadeDeleteAgents deletes every Agent in the given slice. Called
// after user confirmation when --cascade-agents is set. Best-effort:
// failure on one Agent is reported but doesn't abort the rest, so a
// partial cascade doesn't leave the user with an inconsistent state
// they can't undo. Returns the first error encountered.
func cascadeDeleteAgents(ctx context.Context, c ctrlclient.Client, out io.Writer, agents []orchv1alpha1.Agent) error {
	var firstErr error
	for i := range agents {
		a := agents[i]
		if err := c.Delete(ctx, &a); err != nil {
			fmt.Fprintf(out, "  ✗ Failed to delete Agent %s/%s: %v\n", a.Namespace, a.Name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fmt.Fprintf(out, "  ✓ Deleted Agent %s/%s\n", a.Namespace, a.Name)
	}
	return firstErr
}

// refsFromNames converts a []string of names into a []LocalObjectReference.
func refsFromNames(names []string) []orchv1alpha1.LocalObjectReference {
	if len(names) == 0 {
		return nil
	}
	out := make([]orchv1alpha1.LocalObjectReference, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		// Day 22: "team-a/qwen" is a cross-namespace ref (needs an
		// AccessGrant in team-a); a bare "qwen" stays same-namespace.
		ref := orchv1alpha1.LocalObjectReference{Name: n}
		if ns, name, ok := strings.Cut(n, "/"); ok {
			ref = orchv1alpha1.LocalObjectReference{Namespace: strings.TrimSpace(ns), Name: strings.TrimSpace(name)}
		}
		out = append(out, ref)
	}
	return out
}

// pollAgentUntilReady polls the Agent's status.phase. Distinct from Skill's
// version because Agent has WaitingForDeps as an intermediate phase that
// deserves its own log line — the user should see which dep is blocking.
func pollAgentUntilReady(ctx context.Context, c ctrlclient.Client, key types.NamespacedName,
	timeout time.Duration, out, errOut io.Writer) error {
	deadline := time.Now().Add(timeout)
	fmt.Fprintf(out, "Waiting for Agent Ready (timeout %s) ...\n", timeout)
	lastPhase := ""
	for time.Now().Before(deadline) {
		var a orchv1alpha1.Agent
		if err := c.Get(ctx, key, &a); err != nil {
			return fmt.Errorf("poll: %w", err)
		}
		phase := string(a.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		if phase != lastPhase {
			extra := ""
			// When waiting on deps, show the reason from the most recent
			// Progressing condition — that's what tells the user WHICH
			// Model/Skill is blocking.
			if a.Status.Phase == orchv1alpha1.AgentPhaseWaitingForDeps {
				for _, c := range a.Status.Conditions {
					if c.Type == "Progressing" && c.Reason == "AwaitingDependencies" {
						extra = "  (" + c.Message + ")"
					}
				}
			}
			fmt.Fprintf(out, "  phase=%s endpoint=%s%s\n", phase, orDash(a.Status.Endpoint), extra)
			lastPhase = phase
		}
		if a.Status.Phase == orchv1alpha1.AgentPhaseReady {
			fmt.Fprintf(out, "✓ Agent %s/%s is Ready — endpoint: %s\n",
				key.Namespace, key.Name, a.Status.Endpoint)
			fmt.Fprintf(out, "  bound models: %s\n", strings.Join(a.Status.BoundModels, ", "))
			fmt.Fprintf(out, "  bound skills: %s\n", strings.Join(a.Status.BoundSkills, ", "))
			return nil
		}
		if a.Status.Phase == orchv1alpha1.AgentPhaseFailed {
			return fmt.Errorf("agent %s/%s reached Failed phase", key.Namespace, key.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	fmt.Fprintln(errOut, "Timed out waiting for Ready.")
	return fmt.Errorf("timed out waiting for Agent %s/%s to reach Ready",
		key.Namespace, key.Name)
}
