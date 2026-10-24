// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

// bairav skill approve: review the tools an MCP server currently offers and
// pin them (Skill spec.toolsPin). From then on the MCP proxy holds back any
// tool that isn't on the list or whose description / schema changes.
//
// The proxy writes every tools/list it sees to its audit log with a
// fingerprint per tool, so approve reads the Skill's proxy logs; it never
// talks to the MCP server itself.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bairav-foundation/orch-engine/cli/internal/dlp"
	"github.com/bairav-foundation/orch-engine/cli/internal/k8sclient"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

var skillApproveCmd = &cobra.Command{
	Use:   "approve <name>",
	Short: "Review a Skill's MCP tools and pin them (k8s)",
	Long: `Shows the tools the Skill's MCP server currently offers, as seen by its
MCP proxy, and after you confirm, pins them in spec.toolsPin. From then on
the proxy holds back any tool that isn't on the list or whose description or
input schema changes (tool poisoning, "rug pulls"), until you approve again.

Needs the MCP proxy: every remote Skill (--url) has one; an image Skill has
one when it has a DLP policy (--dlp-ref). The proxy must have seen at least
one tools/list: run your agent once, or any MCP client against the Skill.`,
	Example: `  bairav skill approve crm --namespace team-a
  bairav skill approve crm --namespace team-a --yes    # no prompt (CI)`,
	Args: cobra.ExactArgs(1),
	RunE: runSkillApprove,
}

func init() {
	skillCmd.AddCommand(skillApproveCmd)
	skillApproveCmd.Flags().String("namespace", "default", "namespace the Skill lives in")
	skillApproveCmd.Flags().BoolP("yes", "y", false, "approve without asking")
}

func runSkillApprove(cmd *cobra.Command, args []string) error {
	name := args[0]
	namespace, _ := cmd.Flags().GetString("namespace")
	yes, _ := cmd.Flags().GetBool("yes")
	out := cmd.OutOrStdout()
	ctx := context.Background()

	c, err := k8sclient.New()
	if err != nil {
		return err
	}
	var s orchv1alpha1.Skill
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &s); err != nil {
		return fmt.Errorf("get skill %s/%s: %w", namespace, name, err)
	}
	container := "gateway"
	if s.Spec.URL == "" {
		if s.Spec.DLPRef == nil {
			return fmt.Errorf("skill %s has no MCP proxy to enforce a pin: redeploy it with --dlp-ref <policy> (an audit-only policy is enough)", name)
		}
		container = "dlp"
	}

	cs, err := k8sclient.NewClientset()
	if err != nil {
		return err
	}
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "orch.bairavs.com/skill=" + name})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	var logs bytes.Buffer
	tail := int64(5000)
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		rc, err := cs.CoreV1().Pods(namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: container, TailLines: &tail}).Stream(ctx)
		if err != nil {
			continue
		}
		_, _ = io.Copy(&logs, rc)
		_ = rc.Close()
		logs.WriteByte('\n')
	}
	tools := dlp.LatestTools(&logs)
	if len(tools) == 0 {
		return fmt.Errorf("the MCP proxy for %s hasn't seen a tools/list yet: run your agent once (or any MCP client against the Skill), then approve again", name)
	}

	fmt.Fprintf(out, "Tools offered by %s/%s now:\n\n", namespace, name)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tCHANGE\tDESCRIPTION")
	pin := map[string]string{}
	changes := 0
	for _, t := range tools {
		pin[t.Name] = t.SHA256
		change := "same"
		switch old, ok := s.Spec.ToolsPin[t.Name]; {
		case len(s.Spec.ToolsPin) == 0:
			change = "-"
		case !ok:
			change, changes = "NEW", changes+1
		case old != t.SHA256:
			change, changes = "CHANGED", changes+1
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Name, change, oneLine(t.Description, 90))
	}
	_ = tw.Flush()
	for old := range s.Spec.ToolsPin {
		if _, still := pin[old]; !still {
			fmt.Fprintf(out, "  (no longer offered: %s; it leaves the pin)\n", old)
		}
	}
	fmt.Fprintln(out)
	if len(s.Spec.ToolsPin) > 0 && changes == 0 && len(pin) == len(s.Spec.ToolsPin) {
		fmt.Fprintln(out, "✓ Matches the current pin; nothing to approve.")
		return nil
	}
	fmt.Fprintln(out, "Read the descriptions above: the model treats them as instructions.")
	fmt.Fprintf(out, "Approving pins these %d tools for %s.\n", len(pin), name)
	if !confirm(cmd, yes) {
		return nil
	}

	base := s.DeepCopy()
	s.Spec.ToolsPin = pin
	if err := c.Patch(ctx, &s, ctrlclient.MergeFrom(base)); err != nil {
		return fmt.Errorf("update skill: %w", err)
	}
	fmt.Fprintf(out, "✓ Pinned %d tools on %s/%s. The proxy restarts with the new list; anything else is held back.\n", len(pin), namespace, name)
	return nil
}

// oneLine flattens and shortens a description for a table cell.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	if s == "" {
		return "(no description)"
	}
	return s
}
