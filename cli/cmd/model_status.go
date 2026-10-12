// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

// `bairav model status [name]`: every deployed model across local, docker
// and k8s, with what it really serves, any upgrade in flight, and AGE.
//
// `bairav ps` is the whole graph (models + skills + agents); this is the
// model-only view, focused on lifecycle: what's serving, since when, what
// it was upgraded from and how to roll back.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bairav-foundation/orch-engine/cli/internal/compose"
	"github.com/bairav-foundation/orch-engine/cli/internal/k8sclient"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

var modelStatusCmd = &cobra.Command{
	Use:   "status [name]",
	Short: "Show deployed models: what each serves, upgrades in flight, and age",
	Long: `List every deployed model (local, docker and k8s) with the model it is
actually serving, its phase, any upgrade in progress, and its age.

With a name, show the full lifecycle for that deployment: serving vs
wanted model, previous model and the rollback command, conditions.

Note: ` + "`bairav model list`" + ` shows the download cache, not deployments.

Examples:
  bairav model status
  bairav model status support-model
  bairav model status --target k8s --namespace team-a`,
	Args: cobra.MaximumNArgs(1),
	RunE: runModelStatus,
}

func init() {
	modelStatusCmd.Flags().String("target", "", "only this target: local | docker | k8s (default: all)")
	modelStatusCmd.Flags().String("namespace", "", "k8s: only this namespace (default: all namespaces)")
	modelCmd.AddCommand(modelStatusCmd)
}

// modelStatusRow is one deployment, whatever the target.
type modelStatusRow struct {
	target, namespace, name string
	serving, wanted         string // repo (file); wanted differs only mid-upgrade
	phase, upgrade          string
	endpoint, strategy      string
	created                 time.Time
	prev                    string
	prevAt                  time.Time
	conditions              []string
	rollbackHint            string
}

func runModelStatus(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	target, _ := cmd.Flags().GetString("target")
	ns, _ := cmd.Flags().GetString("namespace")
	target = strings.ToLower(strings.TrimSpace(target))
	switch target {
	case "", "local", "docker", "k8s":
	case "compose":
		target = "docker"
	case "kubernetes":
		target = "k8s"
	default:
		return fmt.Errorf("unknown --target %q (use local, docker or k8s)", target)
	}
	if ns != "" && target != "" && target != "k8s" {
		return fmt.Errorf("--namespace only applies to --target k8s")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var rows []modelStatusRow
	var notes []string
	if target == "" || target == "local" {
		r, err := localStatusRows()
		if err != nil {
			notes = append(notes, "local: "+err.Error())
		}
		rows = append(rows, r...)
	}
	if target == "" || target == "docker" {
		r, err := dockerStatusRows(ctx)
		if err != nil {
			notes = append(notes, "docker: "+err.Error())
		}
		rows = append(rows, r...)
	}
	if target == "" || target == "k8s" {
		r, err := k8sStatusRows(ctx, ns)
		if err != nil {
			if target == "k8s" {
				return err
			}
			notes = append(notes, "k8s: not reachable ("+firstLine(err.Error())+")")
		}
		rows = append(rows, r...)
	}

	if len(args) == 1 {
		var match []modelStatusRow
		for _, r := range rows {
			if r.name == args[0] {
				match = append(match, r)
			}
		}
		if len(match) == 0 {
			return fmt.Errorf("no deployed model named %q (see `bairav model status`)", args[0])
		}
		for i, r := range match {
			if i > 0 {
				fmt.Fprintln(out)
			}
			renderStatusDetail(out, r, time.Now())
		}
		return nil
	}

	if len(rows) == 0 {
		fmt.Fprintln(out, "No deployed models. Deploy one with: bairav model deploy <hf-repo>")
		fmt.Fprintln(out, "(`bairav model list` shows downloaded models, which aren't running.)")
	} else {
		renderStatusTable(out, rows, time.Now())
		for _, r := range rows {
			if r.target == "docker" && (r.phase == "Stopped" || strings.HasPrefix(r.phase, "Exited")) {
				fmt.Fprintln(out, "\nStopped docker stacks still hold their port and config; remove with: bairav model undeploy <name>")
				break
			}
		}
	}
	for _, n := range notes {
		fmt.Fprintln(cmd.ErrOrStderr(), "note:", n)
	}
	return nil
}

func renderStatusTable(out io.Writer, rows []modelStatusRow, now time.Time) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.target != b.target {
			return targetOrder(a.target) < targetOrder(b.target)
		}
		if a.namespace != b.namespace {
			return a.namespace < b.namespace
		}
		return a.name < b.name
	})
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTARGET\tNAMESPACE\tSERVING\tPHASE\tUPGRADE\tAGE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.name, r.target, or(r.namespace, "-"),
			r.serving, r.phase, or(r.upgrade, "-"), humanAge(now, r.created))
	}
	_ = tw.Flush()
}

func renderStatusDetail(out io.Writer, r modelStatusRow, now time.Time) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	line := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s:\t%s\n", k, v)
		}
	}
	where := r.target
	if r.namespace != "" {
		where += " (namespace " + r.namespace + ")"
	}
	line("Name", r.name)
	line("Target", where)
	line("Phase", r.phase)
	line("Serving", r.serving)
	if r.wanted != "" && r.wanted != r.serving {
		line("Wanted", r.wanted)
	}
	line("Upgrade", r.upgrade)
	line("Strategy", r.strategy)
	line("Endpoint", r.endpoint)
	line("Age", humanAge(now, r.created))
	if r.prev != "" {
		p := r.prev
		if !r.prevAt.IsZero() {
			p += " (upgraded away " + humanAge(now, r.prevAt) + " ago)"
		}
		line("Previous", p)
		line("Rollback", r.rollbackHint)
	}
	for i, c := range r.conditions {
		k := ""
		if i == 0 {
			k = "Conditions:"
		}
		fmt.Fprintf(tw, "%s\t%s\n", k, c)
	}
	_ = tw.Flush()
}

// ---- collectors ------------------------------------------------------------

func localStatusRows() ([]modelStatusRow, error) {
	locals, err := collectLocal()
	if err != nil {
		return nil, err
	}
	var rows []modelStatusRow
	for _, l := range locals {
		phase := "Ready"
		if l.health != "ok" {
			phase = l.health
		}
		rows = append(rows, modelStatusRow{
			target: "local", name: fmt.Sprintf("%s:%d", lastSegment(l.state.Model), l.state.Port),
			serving: l.state.Model, phase: phase, endpoint: l.state.Endpoint(), created: l.state.StartedAt,
		})
	}
	return rows, nil
}

func dockerStatusRows(ctx context.Context) ([]modelStatusRow, error) {
	stacks, err := listDocker(ctx)
	if err != nil {
		return nil, err
	}
	doors := map[string]compose.Stack{} // host-port endpoint lives on dlp in enforce mode
	for _, s := range stacks {
		if s.Kind == compose.KindModel && s.Endpoint != "" {
			doors[s.Name] = s
		}
	}
	var rows []modelStatusRow
	for _, s := range stacks {
		if s.Kind != compose.KindModel || (s.Role != "" && s.Role != compose.RoleServing) {
			continue
		}
		serving := s.Repo
		if p := s.Labels[compose.LabelProvider]; p != "" {
			serving = p + ":" + s.Repo
		}
		r := modelStatusRow{
			target: "docker", name: s.Name, serving: serving, phase: dockerStatusPhase(s.Status),
			endpoint: doors[s.Name].Endpoint, created: s.Created, strategy: "Recreate (docker)",
			conditions: []string{s.Status},
		}
		if p, err := compose.LoadPrevious(s.Name); err == nil && p.Repo != "" {
			r.prev, r.prevAt = modelLabel(p.Repo, p.File), p.At
			r.rollbackHint = "bairav model rollback " + s.Name
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func k8sStatusRows(ctx context.Context, ns string) ([]modelStatusRow, error) {
	c, err := k8sclient.New()
	if err != nil {
		return nil, err
	}
	var opts []ctrlclient.ListOption
	if ns != "" {
		opts = append(opts, ctrlclient.InNamespace(ns))
	}
	var list orchv1alpha1.ModelList
	if err := c.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	rows := make([]modelStatusRow, 0, len(list.Items))
	for _, m := range list.Items {
		rows = append(rows, k8sStatusRow(m))
	}
	return rows, nil
}

// k8sStatusRow is pure so it can be unit-tested.
func k8sStatusRow(m orchv1alpha1.Model) modelStatusRow {
	wanted := modelLabel(m.Spec.Repo, m.Spec.File)
	r := modelStatusRow{
		target: "k8s", namespace: m.Namespace, name: m.Name, wanted: wanted,
		phase:    phaseOr(string(m.Status.Phase), "Pending"),
		endpoint: m.Status.Endpoint, created: m.CreationTimestamp.Time,
		strategy: orDefault(m.Spec.UpgradeStrategy, orchv1alpha1.ModelUpgradeSurge),
	}
	if m.Spec.IsCloud() { // Day 28: "anthropic:claude-sonnet-4-5"
		r.wanted = m.Spec.Provider + ":" + wanted
	}
	switch {
	case m.Status.ServedRepo != "" && m.Spec.IsCloud() && m.Status.ServedRepo == m.Spec.Repo:
		r.serving = m.Spec.Provider + ":" + m.Status.ServedRepo
	case m.Status.ServedRepo != "":
		r.serving = modelLabel(m.Status.ServedRepo, m.Status.ServedFile)
	case m.Status.Phase == orchv1alpha1.ModelPhaseReady:
		r.serving = r.wanted
		r.upgrade = "? (operator too old)"
	default:
		r.serving = "(none yet)"
	}
	if c := meta.FindStatusCondition(m.Status.Conditions, "Upgrading"); c != nil {
		short := lastSegment(m.Spec.Repo)
		if c.Reason == "UpgradeStalled" {
			r.upgrade = "STALLED → " + short
		} else {
			r.upgrade = "rolling out → " + short
		}
	}
	if p := m.Annotations[annPrevRepo]; p != "" {
		r.prev = modelLabel(p, m.Annotations[annPrevFile])
		if t, err := time.Parse(time.RFC3339, m.Annotations[annUpgradedAt]); err == nil {
			r.prevAt = t
		}
		r.rollbackHint = fmt.Sprintf("bairav model rollback %s --target k8s --namespace %s", m.Name, m.Namespace)
		if r.upgrade == "" && !r.prevAt.IsZero() {
			r.upgrade = "from " + lastSegment(p) + " " + humanAge(time.Now(), r.prevAt) + " ago"
		}
	}
	for _, c := range m.Status.Conditions {
		s := fmt.Sprintf("%s=%s", c.Type, c.Status)
		if c.Reason != "" {
			s += " (" + c.Reason + ")"
		}
		if c.Message != "" {
			s += ": " + c.Message
		}
		r.conditions = append(r.conditions, s)
	}
	return r
}

// ---- helpers ---------------------------------------------------------------

// humanAge formats now-t the way kubectl's AGE column does: 45s, 12m, 5h,
// 3d, 2y. Unknown (zero) times print "-".
func humanAge(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "0s"
	case d < 2*time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < 2*time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 2*365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dy", int(d.Hours()/24/365))
	}
}

// dockerStatusPhase is dockerPhase, but honest about stopped stacks: an
// exited container is "Stopped" / "Exited (137)", not a failed deploy.
func dockerStatusPhase(status string) string {
	if rest, ok := strings.CutPrefix(status, "Exited ("); ok {
		code, _, _ := strings.Cut(rest, ")")
		if code == "0" {
			return "Stopped"
		}
		return "Exited (" + code + ")"
	}
	return dockerPhase(status)
}

func targetOrder(t string) int {
	switch t {
	case "local":
		return 0
	case "docker":
		return 1
	}
	return 2
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
