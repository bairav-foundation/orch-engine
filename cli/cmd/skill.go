// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
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

// skillCmd groups `bairav skill <subcommand>` — deploy, undeploy, list.
//
// A "skill" in BAIRAVS is an MCP server (Model Context Protocol) packaged
// as a container the user provides. We don't write the MCP server; the
// user does, in any language. `bairav skill deploy` takes an image ref
// and reconciles it into a K8s Deployment + Service via the Skill CRD,
// making the MCP endpoint a first-class, discoverable, governable
// cluster citizen.
//
// Design note: the CLI shape mirrors `bairav model *` so anyone who
// learned one already knows the other. Same flags, same defaults, same
// audit/output shapes.
var skillCmd = &cobra.Command{
	Use:   "skill",
	Short: "Manage MCP-server Skills (deploy, undeploy, list)",
	Long: `Deploy MCP servers as first-class Kubernetes resources via the Skill
CRD. Each Skill maps to one Deployment + Service running the user-provided
container, with a health-checked in-cluster endpoint that Agents (and
kubectl exec, and curl-from-a-pod) can hit.

Skills speak MCP over HTTP or SSE. stdio-only servers need a wrapper
container that exposes SSE (deferred to v1.1).`,
}

func init() {
	skillCmd.AddCommand(skillDeployCmd)
	skillCmd.AddCommand(skillUndeployCmd)
	skillCmd.AddCommand(skillListCmd)

	skillDeployCmd.Flags().StringP("name", "n", "",
		"name of the Skill CR (default: derived from <image>)")
	skillDeployCmd.Flags().String("namespace", "default",
		"Kubernetes namespace to deploy into")
	skillDeployCmd.Flags().String("transport", "sse",
		"MCP transport the server speaks: sse | http")
	skillDeployCmd.Flags().Int("port", 3000,
		"port the MCP server listens on inside the container")
	skillDeployCmd.Flags().String("health-path", "/health",
		"HTTP path used by readiness/liveness probes")
	skillDeployCmd.Flags().Bool("wait", true,
		"poll until phase=Ready before returning (use --wait=false to return immediately)")
	skillDeployCmd.Flags().Duration("timeout", 3*time.Minute,
		"max time to wait for phase=Ready when --wait is set")
	skillDeployCmd.Flags().String("target", "",
		"where to deploy: docker (default) | k8s")
	skillDeployCmd.Flags().Int("host-port", 0,
		"docker target only: host port to publish the Skill on (0 = docker picks a free port)")

	skillUndeployCmd.Flags().String("target", "",
		"where the Skill runs: docker (default) | k8s")
	skillUndeployCmd.Flags().String("namespace", "default",
		"namespace the Skill lives in")
	skillUndeployCmd.Flags().BoolP("force", "f", false,
		"skip the confirmation prompt")
	skillUndeployCmd.Flags().Bool("cascade-agents", false,
		"also delete any Agent CRs that reference this Skill. "+
			"Without this flag, dependent Agents remain but move to phase=WaitingForDeps.")

	skillListCmd.Flags().String("target", "", "where to list: docker (default) | k8s")
	skillListCmd.Flags().String("namespace", "default",
		"namespace to list Skills from (use --all-namespaces to list every namespace)")
	skillListCmd.Flags().BoolP("all-namespaces", "A", false,
		"list Skills across every namespace")
}

// -------------------------------------------------------------------------
// skill deploy
// -------------------------------------------------------------------------

var skillDeployCmd = &cobra.Command{
	Use:   "deploy <image>",
	Short: "Deploy a Skill (MCP server) — docker by default, --target k8s for a Skill CR",
	Long: `Create or update a Skill custom resource pointing at the given MCP
server container image. The BAIRAVS operator reconciles it into a
Deployment + Service; polls status.phase until Ready by default.

Example:
  bairav skill deploy bairav/mcp-echo:v1 --target k8s   # reference echo server, see examples/mcp-echo/
  bairav skill deploy ghcr.io/myorg/customer-lookup:latest --port 8000 --transport http --target k8s
  bairav skill deploy ghcr.io/myorg/search:v2 --namespace bairav-skills --wait=false --target k8s

  # Docker target (no cluster): joins the shared 'bairav' network as skill-<name>
  bairav skill deploy bairav/mcp-echo:v1 --name echo --target docker`,
	Args: cobra.ExactArgs(1),
	RunE: runSkillDeploy,
}

func runSkillDeploy(cmd *cobra.Command, args []string) error {
	image := args[0]
	name, _ := cmd.Flags().GetString("name")
	namespace, _ := cmd.Flags().GetString("namespace")
	transport, _ := cmd.Flags().GetString("transport")
	port, _ := cmd.Flags().GetInt("port")
	healthPath, _ := cmd.Flags().GetString("health-path")
	wait, _ := cmd.Flags().GetBool("wait")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	if name == "" {
		name = skillNameFromImage(image)
	}
	out := cmd.OutOrStdout()

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	target, err := resolveCmdTarget(cmd)
	if err != nil {
		return err
	}
	if target == "docker" {
		hostPort, _ := cmd.Flags().GetInt("host-port")
		return deploySkillToDocker(ctx, cmd, name, image, transport, port, hostPort)
	}

	c, err := k8sclient.New()
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Context:    %s\n", k8sclient.CurrentContext())
	fmt.Fprintf(out, "Namespace:  %s\n", namespace)
	fmt.Fprintf(out, "Name:       %s\n", name)
	fmt.Fprintf(out, "Image:      %s\n", image)
	fmt.Fprintf(out, "Transport:  %s\n", transport)
	fmt.Fprintf(out, "Port:       %d\n", port)
	fmt.Fprintln(out)

	// Ensure the target namespace exists — same idempotent pattern as
	// model deploy; kubectl-style autocreate.
	if err := ensureNamespace(ctx, c, namespace, out); err != nil {
		return err
	}

	desired := buildSkillCR(name, namespace, image, transport, healthPath, int32(port))

	var existing orchv1alpha1.Skill
	key := types.NamespacedName{Name: name, Namespace: namespace}
	err = c.Get(ctx, key, &existing)
	switch {
	case apierrors.IsNotFound(err):
		fmt.Fprintf(out, "Creating Skill %s/%s ...\n", namespace, name)
		if err := c.Create(ctx, desired); err != nil {
			return fmt.Errorf("create skill: %w", err)
		}
		fmt.Fprintln(out, "✓ Created")
	case err == nil:
		fmt.Fprintf(out, "Updating existing Skill %s/%s ...\n", namespace, name)
		existing.Spec = desired.Spec
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update skill: %w", err)
		}
		fmt.Fprintln(out, "✓ Updated")
	default:
		return fmt.Errorf("get skill: %w", err)
	}

	if !wait {
		fmt.Fprintln(out, "(--wait=false; not polling for Ready)")
		return nil
	}
	return pollSkillUntilReady(ctx, c, key, timeout, out, cmd.ErrOrStderr())
}

// -------------------------------------------------------------------------
// skill undeploy
// -------------------------------------------------------------------------

var skillUndeployCmd = &cobra.Command{
	Use:   "undeploy <name>",
	Short: "Remove a Skill (docker stack by default, or a Skill CR with --target k8s)",
	Long: `Remove a Skill custom resource. Owner references cascade-delete the
underlying Deployment + Service. Cannot be undone; re-deploy with
'bairav skill deploy'.

Example:
  bairav skill undeploy mcp-filesystem --target k8s
  bairav skill undeploy customer-lookup --namespace bairav-skills --target k8s`,
	Args: cobra.ExactArgs(1),
	RunE: runSkillUndeploy,
}

func runSkillUndeploy(cmd *cobra.Command, args []string) error {
	name := args[0]
	namespace, _ := cmd.Flags().GetString("namespace")
	force, _ := cmd.Flags().GetBool("force")
	cascade, _ := cmd.Flags().GetBool("cascade-agents")
	out := cmd.OutOrStdout()

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	target, err := resolveCmdTarget(cmd)
	if err != nil {
		return err
	}
	if target == "docker" {
		return undeploySkillFromDocker(ctx, cmd, name, force, cascade)
	}

	c, err := k8sclient.New()
	if err != nil {
		return err
	}

	// Same idempotent semantics as model undeploy: if Skill is already
	// gone AND --cascade-agents is set, allow the command through so
	// the user can clean up orphaned Agents. Without cascade, missing
	// Skill is a plain error.
	var s orchv1alpha1.Skill
	skillExists := true
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := c.Get(ctx, key, &s); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get skill: %w", err)
		}
		skillExists = false
		if !cascade {
			return fmt.Errorf("skill %s/%s not found in current context "+
				"(if the Skill is already gone and you want to clean up orphaned Agents, "+
				"re-run with --cascade-agents)", namespace, name)
		}
	}

	if skillExists {
		fmt.Fprintf(out, "About to delete Skill %s/%s (image: %s, phase: %s).\n",
			namespace, name, s.Spec.Image, s.Status.Phase)
	} else {
		fmt.Fprintf(out, "Skill %s/%s already gone. Looking for orphaned Agents to cascade-delete ...\n",
			namespace, name)
	}

	dependents, err := listDependentAgents(ctx, c, "Skill", name, namespace)
	if err != nil {
		return fmt.Errorf("check dependent agents: %w", err)
	}
	if skillExists {
		printDependentAgents(out, "Skill", dependents, cascade)
	} else {
		if len(dependents) == 0 {
			fmt.Fprintln(out, "No dependent Agents remain. Nothing to do.")
			return nil
		}
		fmt.Fprintf(out, "%d Agent(s) still reference this Skill:\n", len(dependents))
		for _, a := range dependents {
			phase := string(a.Status.Phase)
			if phase == "" {
				phase = "Pending"
			}
			fmt.Fprintf(out, "  - %s/%s (%s)\n", a.Namespace, a.Name, phase)
		}
		fmt.Fprintln(out, "These Agents will be deleted (--cascade-agents).")
	}

	if !force {
		fmt.Fprint(out, "Type 'yes' to confirm: ")
		var ans string
		_, _ = fmt.Fscanln(cmd.InOrStdin(), &ans)
		if strings.TrimSpace(strings.ToLower(ans)) != "yes" {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	// Cascade before the Skill itself so a partial cascade doesn't
	// leave the Skill deleted but Agents pointed at a phantom endpoint.
	if cascade && len(dependents) > 0 {
		if err := cascadeDeleteAgents(ctx, c, out, dependents); err != nil {
			return fmt.Errorf("cascade agents: %w", err)
		}
	}

	if skillExists {
		if err := c.Delete(ctx, &s); err != nil {
			return fmt.Errorf("delete skill: %w", err)
		}
		fmt.Fprintf(out, "✓ Deleted Skill %s/%s\n", namespace, name)
	}
	return nil
}

// -------------------------------------------------------------------------
// skill list
// -------------------------------------------------------------------------

var skillListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Skills (docker by default; --target k8s for Skill CRs)",
	RunE:  runSkillList,
}

func runSkillList(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	target, err := resolveCmdTarget(cmd)
	if err != nil {
		return err
	}
	if target == "docker" {
		return listDockerKind(cmd, compose.KindSkill)
	}
	namespace, _ := cmd.Flags().GetString("namespace")
	allNS, _ := cmd.Flags().GetBool("all-namespaces")

	c, err := k8sclient.New()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var list orchv1alpha1.SkillList
	opts := []ctrlclient.ListOption{}
	if !allNS {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}
	if err := c.List(ctx, &list, opts...); err != nil {
		return fmt.Errorf("list skills: %w", err)
	}
	if len(list.Items) == 0 {
		if allNS {
			fmt.Fprintln(out, "No Skills found in any namespace.")
		} else {
			fmt.Fprintf(out, "No Skills found in namespace %q.\n", namespace)
		}
		fmt.Fprintln(out, "Deploy one: bairav skill deploy <image> --target k8s")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tNAME\tIMAGE\tPHASE\tENDPOINT")
	for _, s := range list.Items {
		phase := string(s.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		endpoint := s.Status.Endpoint
		if endpoint == "" {
			endpoint = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.Namespace, s.Name, s.Spec.Image, phase, endpoint)
	}
	_ = tw.Flush()
	return nil
}

// -------------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------------

// buildSkillCR turns CLI args into a v1alpha1 Skill spec.
func buildSkillCR(name, namespace, image, transport, healthPath string, port int32) *orchv1alpha1.Skill {
	return &orchv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "bairav",
			},
		},
		Spec: orchv1alpha1.SkillSpec{
			Image:      image,
			Transport:  orchv1alpha1.SkillTransport(transport),
			Port:       port,
			HealthPath: healthPath,
		},
	}
}

// skillNameRe is the DNS-1123 disallowed-char pattern (same as k8sNameRe).
var skillNameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// skillNameFromImage derives a Skill CR name from a container image ref.
//
//	mcp/filesystem:v1                 -> mcp-filesystem
//	ghcr.io/myorg/customer-lookup:v2  -> customer-lookup
//	registry.example.com:5000/foo/bar -> bar
//	foo:tag                           -> foo
//
// Strategy: take the last "/" segment (drops registry + org), strip
// everything at/after ":" or "@" (drops tag/digest), lowercase, sanitise
// per DNS-1123, cap at 63 chars. Same conceptual shape as
// k8sNameFromRepo, distinct implementation because image refs have
// different structure (registry:port//org/repo:tag@digest) than HF slugs.
func skillNameFromImage(image string) string {
	s := image
	// Strip digest (@sha256:...) if present.
	if idx := strings.Index(s, "@"); idx >= 0 {
		s = s[:idx]
	}
	// Take last "/" segment (drops registry:port and org).
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		s = s[idx+1:]
	}
	// Strip tag (":v1").
	if idx := strings.Index(s, ":"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.ToLower(s)
	s = skillNameRe.ReplaceAllString(s, "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = s[:63]
		s = strings.TrimRight(s, "-")
	}
	if s == "" {
		s = "bairav-skill"
	}
	return s
}

// pollSkillUntilReady blocks until the Skill's status.phase reaches
// Ready, times out, or the context is cancelled. Mirrors pollUntilReady
// for Models but with Skill types and phases; kept as a separate
// function so the two paths can evolve independently.
func pollSkillUntilReady(ctx context.Context, c ctrlclient.Client, key types.NamespacedName,
	timeout time.Duration, out, errOut io.Writer) error {
	deadline := time.Now().Add(timeout)
	fmt.Fprintf(out, "Waiting for Skill Ready (timeout %s) ...\n", timeout)
	lastPhase := ""
	for time.Now().Before(deadline) {
		var s orchv1alpha1.Skill
		if err := c.Get(ctx, key, &s); err != nil {
			return fmt.Errorf("poll: %w", err)
		}
		phase := string(s.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		if phase != lastPhase {
			fmt.Fprintf(out, "  phase=%s endpoint=%s\n", phase, orDash(s.Status.Endpoint))
			lastPhase = phase
		}
		if s.Status.Phase == orchv1alpha1.SkillPhaseReady {
			fmt.Fprintf(out, "✓ Skill %s/%s is Ready — endpoint: %s\n",
				key.Namespace, key.Name, s.Status.Endpoint)
			return nil
		}
		if s.Status.Phase == orchv1alpha1.SkillPhaseFailed {
			return fmt.Errorf("skill %s/%s reached Failed phase", key.Namespace, key.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	fmt.Fprintln(errOut, "Timed out waiting for Ready.")
	return fmt.Errorf("timed out waiting for Skill %s/%s to reach Ready",
		key.Namespace, key.Name)
}

// orDash returns "—" when s is empty, else s. Used in table + poll
// output where blank cells look like a formatting bug.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
