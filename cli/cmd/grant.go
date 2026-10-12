// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

// `bairav grant` / `bairav revoke` / `bairav grants` manage
// AccessGrant CRs, the owner-side half of cross-namespace refs. One grant
// per consumer namespace, named allow-<from>, so repeated `grant` calls
// merge into it and `revoke` knows exactly what to touch. Kubernetes-only:
// the docker target has no namespaces.
//
// Who may run these is the cluster's RBAC on accessgrants — an admin-only
// cluster simply doesn't give team leads create/update/delete on them.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bairav-foundation/orch-engine/cli/internal/k8sclient"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

var grantCmd = &cobra.Command{
	Use:   "grant <from-namespace>",
	Short: "Let Agents in another namespace use Models/Skills in this one (k8s)",
	Long: `Create or extend the AccessGrant "allow-<from-namespace>" in the owner
namespace (-n). Agents in <from-namespace> can then reference the listed
Models/Skills as <owner-namespace>/<name>.

Examples:
  bairav grant team-b --model qwen -n team-a
  bairav grant team-b --model qwen --skill echo -n team-a
  bairav grant team-b --all-models -n shared-models`,
	Args: cobra.ExactArgs(1),
	RunE: runGrant,
}

var revokeCmd = &cobra.Command{
	Use:   "revoke <from-namespace>",
	Short: "Withdraw access granted with `bairav grant` (k8s)",
	Long: `With --model/--skill, removes just those entries from the grant;
without them, deletes the whole grant for <from-namespace>. Agents that
lose access move to WaitingForDeps with a message saying why.

Examples:
  bairav revoke team-b --model qwen -n team-a
  bairav revoke team-b -n team-a`,
	Args: cobra.ExactArgs(1),
	RunE: runRevoke,
}

var grantsCmd = &cobra.Command{
	Use:   "grants",
	Short: "List AccessGrants (k8s)",
	RunE:  runGrants,
}

func init() {
	for _, c := range []*cobra.Command{grantCmd, revokeCmd} {
		c.Flags().StringP("namespace", "n", "default", "owner namespace (where the Models/Skills live)")
		c.Flags().StringSlice("model", nil, "Model name(s)")
		c.Flags().StringSlice("skill", nil, "Skill name(s)")
	}
	grantCmd.Flags().Bool("all-models", false, "grant every Model in the owner namespace")
	grantCmd.Flags().Bool("all-skills", false, "grant every Skill in the owner namespace")
	grantsCmd.Flags().StringP("namespace", "n", "default", "owner namespace to list")
	grantsCmd.Flags().BoolP("all-namespaces", "A", false, "list grants in every namespace")
	rootCmd.AddCommand(grantCmd, revokeCmd, grantsCmd)
}

// grantName is the deterministic grant name per consumer namespace.
func grantName(from string) string { return "allow-" + from }

// grantTargets turns --model/--skill/--all-* into AccessGrantTo entries.
func grantTargets(cmd *cobra.Command) []orchv1alpha1.AccessGrantTo {
	var out []orchv1alpha1.AccessGrantTo
	models, _ := cmd.Flags().GetStringSlice("model")
	skills, _ := cmd.Flags().GetStringSlice("skill")
	for _, m := range cleanNames(models) {
		out = append(out, orchv1alpha1.AccessGrantTo{Kind: orchv1alpha1.AccessGrantKindModel, Name: m})
	}
	for _, s := range cleanNames(skills) {
		out = append(out, orchv1alpha1.AccessGrantTo{Kind: orchv1alpha1.AccessGrantKindSkill, Name: s})
	}
	if f := cmd.Flags().Lookup("all-models"); f != nil && f.Value.String() == "true" {
		out = append(out, orchv1alpha1.AccessGrantTo{Kind: orchv1alpha1.AccessGrantKindModel})
	}
	if f := cmd.Flags().Lookup("all-skills"); f != nil && f.Value.String() == "true" {
		out = append(out, orchv1alpha1.AccessGrantTo{Kind: orchv1alpha1.AccessGrantKindSkill})
	}
	return out
}

// mergeTargets returns existing ∪ add, without duplicates, sorted for a
// stable spec (so re-running the same grant is a no-op update).
func mergeTargets(existing, add []orchv1alpha1.AccessGrantTo) []orchv1alpha1.AccessGrantTo {
	seen := map[orchv1alpha1.AccessGrantTo]bool{}
	var out []orchv1alpha1.AccessGrantTo
	for _, t := range append(append([]orchv1alpha1.AccessGrantTo{}, existing...), add...) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sortTargets(out)
	return out
}

// removeTargets returns existing minus remove.
func removeTargets(existing, remove []orchv1alpha1.AccessGrantTo) []orchv1alpha1.AccessGrantTo {
	drop := map[orchv1alpha1.AccessGrantTo]bool{}
	for _, t := range remove {
		drop[t] = true
	}
	var out []orchv1alpha1.AccessGrantTo
	for _, t := range existing {
		if !drop[t] {
			out = append(out, t)
		}
	}
	return out
}

func sortTargets(ts []orchv1alpha1.AccessGrantTo) {
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Kind != ts[j].Kind {
			return ts[i].Kind < ts[j].Kind
		}
		return ts[i].Name < ts[j].Name
	})
}

func describeTarget(t orchv1alpha1.AccessGrantTo) string {
	if t.Name == "" {
		return string(t.Kind) + "/*"
	}
	return string(t.Kind) + "/" + t.Name
}

func runGrant(cmd *cobra.Command, args []string) error {
	from := strings.TrimSpace(args[0])
	owner, _ := cmd.Flags().GetString("namespace")
	if from == owner {
		return fmt.Errorf("%s is the owner namespace; same-namespace refs never need a grant", from)
	}
	add := grantTargets(cmd)
	if len(add) == 0 {
		return fmt.Errorf("nothing to grant: pass --model, --skill, --all-models or --all-skills")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := k8sclient.New()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	key := types.NamespacedName{Namespace: owner, Name: grantName(from)}

	var g orchv1alpha1.AccessGrant
	err = c.Get(ctx, key, &g)
	switch {
	case apierrors.IsNotFound(err):
		g = orchv1alpha1.AccessGrant{
			ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: owner,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "bairav-cli"},
			},
			Spec: orchv1alpha1.AccessGrantSpec{
				From: []orchv1alpha1.AccessGrantFrom{{Namespace: from}},
				To:   mergeTargets(nil, add),
			},
		}
		if err := c.Create(ctx, &g); err != nil {
			return fmt.Errorf("create accessgrant: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get accessgrant: %w", err)
	default:
		g.Spec.To = mergeTargets(g.Spec.To, add)
		if err := c.Update(ctx, &g); err != nil {
			return fmt.Errorf("update accessgrant: %w", err)
		}
	}
	fmt.Fprintf(out, "✓ %s/%s: %s may now use %s\n", owner, key.Name, from, joinTargets(g.Spec.To))
	fmt.Fprintf(out, "  Agents in %s reference them as %s/<name>, e.g. %s\n",
		from, owner, exampleRef(owner, g.Spec.To))
	return nil
}

func runRevoke(cmd *cobra.Command, args []string) error {
	from := strings.TrimSpace(args[0])
	owner, _ := cmd.Flags().GetString("namespace")
	remove := grantTargets(cmd)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := k8sclient.New()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	key := types.NamespacedName{Namespace: owner, Name: grantName(from)}

	var g orchv1alpha1.AccessGrant
	if err := c.Get(ctx, key, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("no grant %s/%s — %s has no access to revoke", owner, key.Name, from)
		}
		return fmt.Errorf("get accessgrant: %w", err)
	}
	if len(remove) > 0 {
		g.Spec.To = removeTargets(g.Spec.To, remove)
	}
	if len(remove) == 0 || len(g.Spec.To) == 0 {
		if err := c.Delete(ctx, &g); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete accessgrant: %w", err)
		}
		fmt.Fprintf(out, "✓ Deleted %s/%s: %s has no access to %s any more\n", owner, key.Name, from, owner)
		return nil
	}
	if err := c.Update(ctx, &g); err != nil {
		return fmt.Errorf("update accessgrant: %w", err)
	}
	fmt.Fprintf(out, "✓ %s/%s: %s keeps %s\n", owner, key.Name, from, joinTargets(g.Spec.To))
	return nil
}

func runGrants(cmd *cobra.Command, _ []string) error {
	owner, _ := cmd.Flags().GetString("namespace")
	all, _ := cmd.Flags().GetBool("all-namespaces")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := k8sclient.New()
	if err != nil {
		return err
	}
	var list orchv1alpha1.AccessGrantList
	var opts []ctrlclient.ListOption
	if !all {
		opts = append(opts, ctrlclient.InNamespace(owner))
	}
	if err := c.List(ctx, &list, opts...); err != nil {
		return fmt.Errorf("list accessgrants: %w", err)
	}
	out := cmd.OutOrStdout()
	if len(list.Items) == 0 {
		fmt.Fprintln(out, "No AccessGrants. Create one: bairav grant <from-namespace> --model <name> -n <owner-namespace>")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "OWNER\tNAME\tFROM\tTO")
	for _, g := range list.Items {
		froms := make([]string, 0, len(g.Spec.From))
		for _, f := range g.Spec.From {
			froms = append(froms, f.Namespace)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", g.Namespace, g.Name, strings.Join(froms, ","), joinTargets(g.Spec.To))
	}
	return tw.Flush()
}

func joinTargets(ts []orchv1alpha1.AccessGrantTo) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, describeTarget(t))
	}
	return strings.Join(parts, ", ")
}

// exampleRef renders a ready-to-paste agent flag for the first named
// target, e.g. "--model team-a/qwen".
func exampleRef(owner string, ts []orchv1alpha1.AccessGrantTo) string {
	for _, t := range ts {
		if t.Name == "" {
			continue
		}
		flag := "--model"
		if t.Kind == orchv1alpha1.AccessGrantKindSkill {
			flag = "--skill"
		}
		return fmt.Sprintf("%s %s/%s", flag, owner, t.Name)
	}
	return "--model " + owner + "/<name>"
}
