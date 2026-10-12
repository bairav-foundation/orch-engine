// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

// Day 20: --target docker for `skill` and `agent` deploy/undeploy, plus
// docker-side dependent handling for `model undeploy`.
//
// Day 21: model, skill and agent all share one default target (docker) via
// resolveTarget, and all print the same banner when --target is omitted.
// Two different defaults across sibling commands was a footgun.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/bairav-foundation/orch-engine/cli/internal/compose"
)

// resolveCmdTarget reads --target from cmd, resolves it with the same rule
// model uses (empty → docker), and prints the shared default banner when
// the flag was omitted. Also rejects target-specific flags used with the
// wrong target, so `--namespace` on docker or `--host-port` on k8s fails
// loudly instead of being silently ignored.
func resolveCmdTarget(cmd *cobra.Command) (string, error) {
	raw, _ := cmd.Flags().GetString("target")
	target, err := resolveTarget(raw)
	if err != nil {
		return "", err
	}
	if raw == "" {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"--target not specified; defaulting to docker. "+
				"Pass --target k8s to use your active kubeconfig context.")
	}
	wrong := k8sOnlyFlags
	if target == "k8s" {
		wrong = dockerOnlyFlags
	}
	for _, f := range wrong {
		if cmd.Flags().Lookup(f) != nil && cmd.Flags().Changed(f) {
			return "", fmt.Errorf("--%s doesn't apply to --target %s", f, target)
		}
	}
	return target, nil
}

// k8sOnlyFlags / dockerOnlyFlags are the skill/agent flags that only mean
// something on one target. Flags a command doesn't define are skipped.
var (
	k8sOnlyFlags    = []string{"namespace", "wait", "timeout", "dlp-policy", "all-namespaces"}
	dockerOnlyFlags = []string{"host-port", "dlp", "dlp-pack", "dlp-action", "dlp-direction", "dlp-policy-file"}
)

func listDocker(ctx context.Context) ([]compose.Stack, error) {
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return compose.List(lctx)
}

// confirm asks "Type 'yes'" unless force. Returns true to proceed.
func confirm(cmd *cobra.Command, force bool) bool {
	if force {
		return true
	}
	out := cmd.OutOrStdout()
	fmt.Fprint(out, "Type 'yes' to confirm: ")
	var ans string
	_, _ = fmt.Fscanln(cmd.InOrStdin(), &ans)
	if strings.TrimSpace(strings.ToLower(ans)) != "yes" {
		fmt.Fprintln(out, "Aborted.")
		return false
	}
	return true
}

// -------------------------------------------------------------------------
// skill --target docker
// -------------------------------------------------------------------------

func deploySkillToDocker(ctx context.Context, cmd *cobra.Command, name, image, transport string, port, hostPort int) error {
	out := cmd.OutOrStdout()
	spec := compose.SkillSpec{
		Name: name, Image: image, Transport: transport, Port: port, HostPort: hostPort,
	}
	fmt.Fprintf(out, "Target:     docker (compose)\n")
	fmt.Fprintf(out, "Name:       %s\n", name)
	fmt.Fprintf(out, "Image:      %s\n", image)
	fmt.Fprintf(out, "Transport:  %s\n", transport)
	fmt.Fprintf(out, "Network:    %s (alias %s)\n", compose.SharedNetwork, compose.SkillNetAlias(name))
	fmt.Fprintf(out, "In-network: %s\n\n", compose.SkillNetEndpoint(name, port))
	if err := compose.UpSkill(ctx, spec, out, cmd.ErrOrStderr()); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n✓ Skill stack up. Host port: see `bairav ps`.\n")
	fmt.Fprintf(out, "  Wire it into an agent: bairav agent deploy <image> --target docker --skill %s\n", name)
	return nil
}

func undeploySkillFromDocker(ctx context.Context, cmd *cobra.Command, name string, force, cascade bool) error {
	return undeployDockerWithDependents(ctx, cmd, compose.KindSkill, compose.LabelRefSkillPrefix,
		compose.SkillStackName(name), name, force, cascade)
}

// -------------------------------------------------------------------------
// agent --target docker
// -------------------------------------------------------------------------

type agentDockerOpts struct {
	name, image       string
	port, hostPort    int
	models, skills    []string
	dlp               bool
	dlpPacks          []string
	dlpAction, dlpDir string
	dlpPolicyFile     string
}

func deployAgentToDocker(ctx context.Context, cmd *cobra.Command, o agentDockerOpts) error {
	out := cmd.OutOrStdout()
	stacks, err := listDocker(ctx)
	if err != nil {
		return fmt.Errorf("list docker stacks: %w", err)
	}

	spec := compose.AgentSpec{Name: o.name, Image: o.image, Port: o.port, HostPort: o.hostPort}
	for _, r := range append(cleanNames(o.models), cleanNames(o.skills)...) {
		if strings.Contains(r, "/") {
			return fmt.Errorf("ref %q: namespace/name refs are k8s-only (docker has no namespaces); use --target k8s", r)
		}
	}
	var problems []string
	for _, m := range cleanNames(o.models) {
		if door := compose.FindDoor(stacks, compose.KindModel, m); door != nil {
			spec.Models = append(spec.Models, compose.ResolvedModel{Name: m, Endpoint: door.NetEndpoint})
			continue
		}
		problems = append(problems, missingRefHint(stacks, compose.KindModel, m))
	}
	for _, s := range cleanNames(o.skills) {
		if door := compose.FindDoor(stacks, compose.KindSkill, s); door != nil {
			spec.Skills = append(spec.Skills, compose.ResolvedSkill{
				Name: s, Endpoint: door.NetEndpoint, Transport: door.Labels[compose.LabelTransport],
			})
			continue
		}
		problems = append(problems, missingRefHint(stacks, compose.KindSkill, s))
	}
	if len(problems) > 0 {
		// No controller on the docker target, so no WaitingForDeps — fail
		// fast with the fix instead of starting a half-wired container.
		return fmt.Errorf("cannot resolve agent refs on the docker target:\n  - %s",
			strings.Join(problems, "\n  - "))
	}

	if o.dlp {
		d := &compose.AgentDLP{Packs: o.dlpPacks, Action: o.dlpAction, Direction: o.dlpDir}
		if o.dlpPolicyFile != "" {
			abs, err := filepath.Abs(o.dlpPolicyFile)
			if err != nil {
				return fmt.Errorf("--dlp-policy-file: %w", err)
			}
			if _, err := os.Stat(abs); err != nil {
				return fmt.Errorf("--dlp-policy-file: %w", err)
			}
			d.PolicyHostPath = abs
		}
		spec.DLP = d
	}

	fmt.Fprintf(out, "Target:     docker (compose)\n")
	fmt.Fprintf(out, "Name:       %s\n", o.name)
	fmt.Fprintf(out, "Image:      %s\n", o.image)
	endpoints := compose.AgentModelEndpoints(spec)
	for _, m := range spec.Models {
		fmt.Fprintf(out, "Model:      %s → %s\n", m.Name, endpoints[m.Name])
	}
	for _, s := range spec.Skills {
		fmt.Fprintf(out, "Skill:      %s → %s (%s)\n", s.Name, s.Endpoint, s.Transport)
	}
	if spec.DLP != nil {
		packs := "none (built-ins only)"
		if len(spec.DLP.Packs) > 0 {
			packs = strings.Join(spec.DLP.Packs, ",")
		}
		fmt.Fprintf(out, "DLP:        one proxy per Model; packs=%s action=%s direction=%s\n",
			packs, orDefault(spec.DLP.Action, "redact"), orDefault(spec.DLP.Direction, "both"))
	}
	fmt.Fprintln(out)

	if err := compose.UpAgent(ctx, spec, out, cmd.ErrOrStderr()); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n✓ Agent stack up. Host port: see `bairav ps`.\n")
	fmt.Fprintf(out, "  Note: refs were resolved at deploy time. If a Model/Skill stack is\n")
	fmt.Fprintf(out, "        redeployed or removed, re-run this command (no live re-resolution on docker).\n")
	return nil
}

func undeployAgentFromDocker(ctx context.Context, cmd *cobra.Command, name string, force bool) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "About to tear down docker stack 'bairav-%s'.\n", compose.AgentStackName(name))
	if !confirm(cmd, force) {
		return nil
	}
	if err := compose.Down(ctx, compose.AgentStackName(name), out, cmd.ErrOrStderr()); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ Agent '%s' torn down.\n", name)
	return nil
}

// -------------------------------------------------------------------------
// shared: dependents on the docker target
// -------------------------------------------------------------------------

// undeployDockerWithDependents tears down a Model or Skill stack after
// listing (and optionally cascading) the Agent stacks that reference it.
// Mirrors the K8s semantics from Day 15: always show dependents; cascade
// only with --cascade-agents; cascade works even if the target is gone.
func undeployDockerWithDependents(ctx context.Context, cmd *cobra.Command, kind, refPrefix, stackName, name string, force, cascade bool) error {
	out := cmd.OutOrStdout()
	stacks, err := listDocker(ctx)
	if err != nil {
		return fmt.Errorf("list docker stacks: %w", err)
	}
	exists := compose.FindAny(stacks, kind, name) != nil
	dependents := compose.DependentAgents(stacks, refPrefix, name)

	if !exists && !cascade {
		return fmt.Errorf("%s %q not found on the docker target "+
			"(if it's already gone and you want to clean up orphaned Agents, re-run with --cascade-agents)", kind, name)
	}
	if exists {
		fmt.Fprintf(out, "About to tear down docker stack 'bairav-%s'.\n", stackName)
	} else {
		fmt.Fprintf(out, "%s %q already gone. Looking for orphaned Agents to cascade-delete ...\n", kind, name)
	}
	printDockerDependents(out, kind, dependents, cascade)
	if !exists && len(dependents) == 0 {
		fmt.Fprintln(out, "Nothing to do.")
		return nil
	}
	if !confirm(cmd, force) {
		return nil
	}

	if cascade {
		// Agents first, so a partial failure never leaves Agents pointing at
		// a stack we already removed (same ordering as the K8s path).
		var firstErr error
		for _, a := range dependents {
			if err := compose.Down(ctx, compose.AgentStackName(a), out, cmd.ErrOrStderr()); err != nil {
				fmt.Fprintf(out, "  ✗ Agent %s: %v\n", a, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			fmt.Fprintf(out, "  ✓ Deleted Agent %s\n", a)
		}
		if firstErr != nil {
			return fmt.Errorf("cascade delete: %w", firstErr)
		}
	}
	if exists {
		if err := compose.Down(ctx, stackName, out, cmd.ErrOrStderr()); err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ Stack 'bairav-%s' torn down.\n", stackName)
	}
	return nil
}

func printDockerDependents(out io.Writer, kind string, deps []string, cascade bool) {
	if len(deps) == 0 {
		return
	}
	fmt.Fprintf(out, "%d Agent(s) reference this %s:\n", len(deps), kind)
	for _, d := range deps {
		fmt.Fprintf(out, "  - %s\n", d)
	}
	if cascade {
		fmt.Fprintln(out, "These Agents will ALSO be deleted (--cascade-agents).")
	} else {
		fmt.Fprintln(out, "These Agents will keep running with a dead endpoint (no controller on docker).")
		fmt.Fprintln(out, "Pass --cascade-agents to delete them too.")
	}
}

// missingRefHint explains why a ref didn't resolve, with the fix.
func missingRefHint(stacks []compose.Stack, kind, name string) string {
	if compose.FindAny(stacks, kind, name) != nil {
		return fmt.Sprintf("%s %q is running but predates the shared network (Day 20) — redeploy it: bairav %s deploy ... --target docker", kind, name, kind)
	}
	return fmt.Sprintf("%s %q is not deployed on docker — run: bairav %s deploy ... --target docker", kind, name, kind)
}

// listDockerKind prints the docker-target Skills or Agents (door
// containers only; an agent's DLP proxies are shown by `bairav ps`).
func listDockerKind(cmd *cobra.Command, kind string) error {
	out := cmd.OutOrStdout()
	stacks, err := listDocker(context.Background())
	if err != nil {
		return fmt.Errorf("list docker stacks: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIMAGE\tSTATUS\tHOST ENDPOINT\tIN-NETWORK ENDPOINT")
	n := 0
	for _, s := range stacks {
		if s.Kind != kind || s.Role == compose.RoleDLP {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Image, s.Status, orDefault(s.Endpoint, "—"), orDefault(s.NetEndpoint, "—"))
		n++
	}
	if n == 0 {
		fmt.Fprintf(out, "No %ss on the docker target. (Use --target k8s for the cluster.)\n", kind)
		return nil
	}
	return tw.Flush()
}

// cleanNames trims whitespace and drops blanks (same rule as refsFromNames).
func cleanNames(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
