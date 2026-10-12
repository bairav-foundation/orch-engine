// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

// Day 27: `bairav model upgrade` and `bairav model rollback`.
//
// The Model keeps its name, so its Service (and every Agent's
// BAIRAV_MODEL_<X>_ENDPOINT) stays the same; only what serves behind it
// changes. On k8s the operator rolls out with surge (new pod Ready before
// the old one goes) unless --strategy recreate. The CLI then waits for
// status.servedRepo to flip, sends a smoke prompt through the API server's
// service proxy, and restores the previous model if anything fails.
//
// The docker target has no rolling update: the container is recreated (a
// short outage), health-checked and smoke-tested, with the same automatic
// rollback.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bairav-foundation/orch-engine/cli/internal/compose"
	"github.com/bairav-foundation/orch-engine/cli/internal/k8sclient"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// Annotations recording the model a k8s Model ran before its last upgrade.
const (
	annPrevRepo   = "orch.bairavs.com/previous-repo"
	annPrevFile   = "orch.bairavs.com/previous-file"
	annUpgradedAt = "orch.bairavs.com/upgraded-at"
	// annPrevSpec is the full previous modelRef as JSON (Day 28: provider
	// and cloud config too, so rollback can cross local ⇄ cloud).
	annPrevSpec = "orch.bairavs.com/previous-spec"
)

var modelUpgradeCmd = &cobra.Command{
	Use:   "upgrade <name> --to <hf-repo | provider-model-id>",
	Short: "Swap the model behind a deployment safely, with automatic rollback",
	Long: `Replace the model served by an existing deployment. The name, endpoint
and every Agent wired to it stay the same.

k8s:    the new model starts next to the old one and takes over only once
        Ready (needs memory for both). --strategy recreate stops the old
        one first instead (brief outage, half the memory).
docker: the container is recreated (brief outage).

Both: wait until the new model serves, send a smoke prompt, and roll back
to the previous model automatically if either step fails.

--provider swaps between local and cloud models (Day 28). Without it the
deployment keeps its current provider.

Examples:
  bairav model upgrade support-model --to Qwen/Qwen2.5-1.5B-Instruct-GGUF --provider local   # GPT → Qwen
  bairav model upgrade support-model --to claude-sonnet-4-5 --provider anthropic          # → Claude
  bairav model upgrade support-model --to Qwen/Qwen2.5-1.5B-Instruct-GGUF --target k8s --namespace team-a
  bairav model upgrade qwen --to Qwen/Qwen2.5-1.5B-Instruct-GGUF                 # docker
  bairav model rollback support-model --target k8s --namespace team-a`,
	Args: cobra.ExactArgs(1),
	RunE: runModelUpgrade,
}

var modelRollbackCmd = &cobra.Command{
	Use:   "rollback <name>",
	Short: "Go back to the model a deployment ran before its last upgrade",
	Args:  cobra.ExactArgs(1),
	RunE:  runModelRollback,
}

func init() {
	for _, c := range []*cobra.Command{modelUpgradeCmd, modelRollbackCmd} {
		c.Flags().String("target", "", "where the model runs: docker (default) | k8s")
		c.Flags().String("namespace", "default", "k8s target: namespace of the Model")
		c.Flags().Duration("rollout-timeout", 30*time.Minute, "how long to wait for the new model to serve")
		c.Flags().Bool("no-rollback", false, "on failure, leave the new spec in place instead of restoring the old model")
		c.Flags().String("smoke-prompt", "Reply with the single word: OK", "prompt sent to the new model before declaring success")
		c.Flags().Bool("no-smoke", false, "skip the smoke prompt (only wait for Ready)")
	}
	modelUpgradeCmd.Flags().String("to", "", "Hugging Face repo of the new model (required)")
	modelUpgradeCmd.Flags().String("file", "", "GGUF file inside the repo (default: smallest)")
	modelUpgradeCmd.Flags().String("strategy", "", "k8s target: surge (default, zero downtime) | recreate (less memory)")
	addCloudFlags(modelUpgradeCmd)
	modelCmd.AddCommand(modelUpgradeCmd, modelRollbackCmd)
}

// upgradeOpts carries the flags shared by upgrade and rollback.
type upgradeOpts struct {
	name, namespace, repo, file, strategy string
	timeout                               time.Duration
	rollback, smoke                       bool
	prompt                                string
	cloud                                 cloudOpts // upgrade only; cloud.changed = switch/reconfigure provider
}

func readUpgradeOpts(cmd *cobra.Command, name string) (upgradeOpts, error) {
	o := upgradeOpts{name: name}
	o.namespace, _ = cmd.Flags().GetString("namespace")
	o.timeout, _ = cmd.Flags().GetDuration("rollout-timeout")
	noRollback, _ := cmd.Flags().GetBool("no-rollback")
	noSmoke, _ := cmd.Flags().GetBool("no-smoke")
	o.rollback, o.smoke = !noRollback, !noSmoke
	o.prompt, _ = cmd.Flags().GetString("smoke-prompt")
	if f := cmd.Flags().Lookup("strategy"); f != nil {
		switch strings.ToLower(f.Value.String()) {
		case "":
		case "surge":
			o.strategy = orchv1alpha1.ModelUpgradeSurge
		case "recreate":
			o.strategy = orchv1alpha1.ModelUpgradeRecreate
		default:
			return o, fmt.Errorf("unknown --strategy %q (use surge | recreate)", f.Value.String())
		}
	}
	return o, nil
}

func runModelUpgrade(cmd *cobra.Command, args []string) error {
	o, err := readUpgradeOpts(cmd, args[0])
	if err != nil {
		return err
	}
	o.repo, _ = cmd.Flags().GetString("to")
	o.file, _ = cmd.Flags().GetString("file")
	if strings.TrimSpace(o.repo) == "" {
		return fmt.Errorf("--to <hf-repo | provider-model-id> is required")
	}
	if o.cloud, err = readCloudOpts(cmd); err != nil {
		return err
	}
	if o.cloud.isCloud() && o.file != "" {
		return fmt.Errorf("--file is for local models")
	}
	return dispatchUpgrade(cmd, o)
}

func runModelRollback(cmd *cobra.Command, args []string) error {
	o, err := readUpgradeOpts(cmd, args[0])
	if err != nil {
		return err
	}
	o.repo = "" // filled from the recorded previous model
	return dispatchUpgrade(cmd, o)
}

func dispatchUpgrade(cmd *cobra.Command, o upgradeOpts) error {
	target, err := resolveCmdTarget(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if target == "k8s" {
		return upgradeK8s(ctx, cmd, o)
	}
	if o.strategy != "" {
		return fmt.Errorf("--strategy is k8s only; the docker target always recreates the container")
	}
	return upgradeDocker(ctx, cmd, o)
}

// -----------------------------------------------------------------------------
// k8s
// -----------------------------------------------------------------------------

func upgradeK8s(ctx context.Context, cmd *cobra.Command, o upgradeOpts) error {
	out := cmd.OutOrStdout()
	c, err := k8sclient.New()
	if err != nil {
		return err
	}
	key := types.NamespacedName{Namespace: o.namespace, Name: o.name}
	var m orchv1alpha1.Model
	if err := c.Get(ctx, key, &m); err != nil {
		return fmt.Errorf("get model %s/%s: %w", o.namespace, o.name, err)
	}

	cur := refOf(m.Spec)
	var target modelRef
	if o.repo == "" { // rollback
		if js := m.Annotations[annPrevSpec]; js != "" {
			if err := json.Unmarshal([]byte(js), &target); err != nil {
				return fmt.Errorf("read %s annotation: %w", annPrevSpec, err)
			}
		} else { // recorded before Day 28: same provider
			target = modelRef{Repo: m.Annotations[annPrevRepo], File: m.Annotations[annPrevFile], Provider: cur.Provider, Cloud: cur.Cloud}
		}
		if target.Repo == "" {
			return fmt.Errorf("model %s/%s has no recorded previous model (rollback needs a prior `bairav model upgrade`)", o.namespace, o.name)
		}
	} else {
		target = modelRef{Repo: o.repo, File: o.file, Provider: cur.Provider, Cloud: cur.Cloud}
		if o.cloud.changed {
			spec := m.Spec.DeepCopy()
			if err := applyCloudK8s(ctx, c, o.namespace, o.name, o.cloud, spec, out); err != nil {
				return err
			}
			target.Provider, target.Cloud = spec.Provider, spec.Cloud
		}
		if target.isCloud() && target.File != "" {
			return fmt.Errorf("--file is for local models")
		}
	}
	from, to := cur.label(), target.label()
	if cur.same(target) {
		fmt.Fprintf(out, "Model %s/%s already runs %s; nothing to do.\n", o.namespace, o.name, to)
		return nil
	}
	if err := requireServedStatus(ctx, c, key, &m); err != nil {
		return err
	}
	strategy := o.strategy
	if strategy == "" {
		strategy = orDefault(m.Spec.UpgradeStrategy, orchv1alpha1.ModelUpgradeSurge)
	}
	fmt.Fprintf(out, "Model:     %s/%s (context %s)\n", o.namespace, o.name, k8sclient.CurrentContext())
	fmt.Fprintf(out, "From:      %s\nTo:        %s\nStrategy:  %s\n", from, to, strategy)
	switch {
	case cur.isCloud() && target.isCloud():
		fmt.Fprintln(out, "           (gateway pods are tiny; the swap takes seconds)")
	case strategy == orchv1alpha1.ModelUpgradeSurge:
		fmt.Fprintln(out, "           (the old model keeps serving until the new one is Ready; both need memory meanwhile)")
	default:
		fmt.Fprintln(out, "           (the old model stops first: expect a short outage)")
	}
	fmt.Fprintln(out)

	record := map[string]string{
		annPrevRepo: cur.Repo, annPrevFile: cur.File, annPrevSpec: cur.encode(),
		annUpgradedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := setModelSpec(ctx, c, key, target, o.strategy, record); err != nil {
		return err
	}
	fmt.Fprintln(out, "✓ Spec updated; waiting for the new model to take over ...")

	failure := waitServed(ctx, c, key, target.Repo, target.File, o.timeout, out)
	if failure == nil && o.smoke {
		fmt.Fprintln(out, "Smoke test through the model's Service ...")
		failure = smokeK8s(ctx, o.namespace, o.name, o.prompt, out)
	}
	if failure == nil {
		fmt.Fprintf(out, "\n✓ %s/%s now serves %s. Agents were not restarted; their endpoint is unchanged.\n", o.namespace, o.name, to)
		fmt.Fprintf(out, "  Undo with: bairav model rollback %s --target k8s --namespace %s\n", o.name, o.namespace)
		return nil
	}

	fmt.Fprintf(out, "\n✗ Upgrade failed: %v\n", failure)
	if !o.rollback {
		return fmt.Errorf("upgrade failed (left in place because of --no-rollback): %w", failure)
	}
	fmt.Fprintf(out, "Rolling back to %s ...\n", from)
	// Put the annotations back exactly as they were: a failed attempt must
	// not become the "previous model" a later `rollback` would restore.
	original := map[string]string{
		annPrevRepo: m.Annotations[annPrevRepo], annPrevFile: m.Annotations[annPrevFile],
		annPrevSpec: m.Annotations[annPrevSpec], annUpgradedAt: m.Annotations[annUpgradedAt],
	}
	if err := setModelSpec(ctx, c, key, cur, "", original); err != nil {
		return fmt.Errorf("upgrade failed (%v) AND rollback failed: %w", failure, err)
	}
	if err := waitServed(ctx, c, key, cur.Repo, cur.File, o.timeout, out); err != nil {
		return fmt.Errorf("upgrade failed (%v); rollback started but not confirmed: %w", failure, err)
	}
	return fmt.Errorf("upgrade to %s failed and was rolled back; %s is serving again: %w", to, from, failure)
}

// requireServedStatus makes sure the operator fills status.servedRepo
// before we change anything. An operator (or CRD) from before Day 27 never
// sets it, and the upgrade would then wait forever for a field that can't
// change. A freshly reconciled Model gets it within seconds, so give the
// operator a short grace period first.
func requireServedStatus(ctx context.Context, c ctrlclient.Client, key types.NamespacedName, m *orchv1alpha1.Model) error {
	deadline := time.Now().Add(20 * time.Second)
	for m.Status.ServedRepo == "" {
		if time.Now().After(deadline) {
			if m.Status.Phase != orchv1alpha1.ModelPhaseReady {
				return fmt.Errorf("model %s/%s is %s, not Ready; wait for it to serve before upgrading", key.Namespace, key.Name, orDefault(string(m.Status.Phase), "not reconciled yet"))
			}
			return fmt.Errorf("model %s/%s is Ready but status.servedRepo is empty: the operator or CRD in this cluster predates `model upgrade`.\n"+
				"Update both, then retry:\n"+
				"  make install-operator\n"+
				"  kubectl -n bairav-system rollout restart deploy/bairav-operator", key.Namespace, key.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if err := c.Get(ctx, key, m); err != nil {
			return fmt.Errorf("get model: %w", err)
		}
	}
	return nil
}

// setModelSpec points the Model at ref (repo, file, provider, cloud) and
// sets the upgrade-record annotations (an empty value removes the key).
func setModelSpec(ctx context.Context, c ctrlclient.Client, key types.NamespacedName, ref modelRef, strategy string, ann map[string]string) error {
	var m orchv1alpha1.Model
	if err := c.Get(ctx, key, &m); err != nil {
		return err
	}
	m.Spec.Repo, m.Spec.File = ref.Repo, ref.File
	m.Spec.Provider, m.Spec.Cloud = orDefault(ref.Provider, providerLocal), ref.Cloud
	if strategy != "" {
		m.Spec.UpgradeStrategy = strategy
	}
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	for k, v := range ann {
		if v == "" {
			delete(m.Annotations, k)
		} else {
			m.Annotations[k] = v
		}
	}
	if err := c.Update(ctx, &m); err != nil {
		return fmt.Errorf("update model: %w", err)
	}
	return nil
}

// waitServed polls until status.servedRepo/servedFile match, the operator
// reports UpgradeStalled, or the timeout passes. Progress lines are printed
// only when the message changes.
func waitServed(ctx context.Context, c ctrlclient.Client, key types.NamespacedName, repo, file string, timeout time.Duration, out io.Writer) error {
	deadline := time.Now().Add(timeout)
	last := ""
	cs, _ := k8sclient.NewClientset() // pod diagnostics are best effort
	var unschedSince time.Time
	for {
		var m orchv1alpha1.Model
		if err := c.Get(ctx, key, &m); err != nil {
			return fmt.Errorf("get model: %w", err)
		}
		if m.Status.ObservedGeneration >= m.Generation && m.Status.ServedRepo == repo && m.Status.ServedFile == file {
			fmt.Fprintf(out, "  ✓ serving %s\n", modelLabel(repo, file))
			return nil
		}
		msg := string(m.Status.Phase)
		if c := meta.FindStatusCondition(m.Status.Conditions, "Available"); c != nil && c.Reason == "InvalidSpec" && m.Status.ObservedGeneration >= m.Generation {
			return fmt.Errorf("operator rejected the spec: %s", c.Message)
		}
		if cond := meta.FindStatusCondition(m.Status.Conditions, "Upgrading"); cond != nil {
			if cond.Reason == "UpgradeStalled" {
				return fmt.Errorf("%s", cond.Message)
			}
			msg = cond.Message
		}
		if cs != nil {
			pods, err := cs.CoreV1().Pods(key.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=bairav-serving,app.kubernetes.io/instance=" + key.Name,
			})
			if err == nil {
				p := podProblem(pods.Items)
				switch {
				case p.fatal:
					return fmt.Errorf("%s", p.msg)
				case p.unschedulable:
					if unschedSince.IsZero() {
						unschedSince = time.Now()
					}
					if time.Since(unschedSince) > 90*time.Second {
						return fmt.Errorf("%s", p.msg)
					}
					msg = p.msg + " (waiting a bit in case a node frees up)"
				default:
					unschedSince = time.Time{}
				}
			}
		}
		if msg != last {
			fmt.Fprintf(out, "  … %s\n", msg)
			last = msg
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s to serve", timeout, modelLabel(repo, file))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// podIssue is what podProblem found wrong with a Model's pods.
type podIssue struct {
	msg           string
	fatal         bool // won't fix itself: fail now (and roll back)
	unschedulable bool // may fix itself if capacity appears
}

// podProblem spots the failures K8s would otherwise retry silently until
// the 30-minute progress deadline: an image the node can't pull, a
// container crash-looping, or a pod no node has room for (common with
// Surge, which needs memory for two models at once).
func podProblem(pods []corev1.Pod) podIssue {
	for _, p := range pods {
		for _, cst := range p.Status.ContainerStatuses {
			w := cst.State.Waiting
			if w == nil {
				continue
			}
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				return podIssue{fatal: true, msg: fmt.Sprintf("pod %s can't pull image %s (%s). "+
					"On k3d, import local images into the cluster: make image-serving (or k3d image import %s)", p.Name, cst.Image, w.Reason, cst.Image)}
			case "CrashLoopBackOff":
				if cst.RestartCount >= 3 {
					return podIssue{fatal: true, msg: fmt.Sprintf("pod %s is crash-looping (%d restarts); see: kubectl logs -n %s %s --previous", p.Name, cst.RestartCount, p.Namespace, p.Name)}
				}
			case "CreateContainerConfigError":
				return podIssue{fatal: true, msg: fmt.Sprintf("pod %s: %s", p.Name, w.Message)}
			}
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == corev1.PodReasonUnschedulable {
				return podIssue{unschedulable: true, msg: fmt.Sprintf("pod %s can't be scheduled: %s. With Surge both models must fit at once; try --strategy recreate", p.Name, cond.Message)}
			}
		}
	}
	return podIssue{}
}

// smokeK8s sends one chat completion through the API server's service
// proxy, so it works from a laptop without port-forwarding.
func smokeK8s(ctx context.Context, ns, name, prompt string, out io.Writer) error {
	cs, err := k8sclient.NewClientset()
	if err != nil {
		return err
	}
	body, err := smokeBody(prompt)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	raw, err := cs.CoreV1().RESTClient().Post().
		AbsPath("/api/v1/namespaces", ns, "services", name+":http", "proxy", "v1", "chat", "completions").
		SetHeader("Content-Type", "application/json").Body(body).DoRaw(sctx)
	if err != nil {
		return fmt.Errorf("smoke prompt: %w", err)
	}
	return checkSmoke(raw, out)
}

// -----------------------------------------------------------------------------
// docker
// -----------------------------------------------------------------------------

func upgradeDocker(ctx context.Context, cmd *cobra.Command, o upgradeOpts) error {
	out := cmd.OutOrStdout()
	cur, err := compose.LoadSpec(o.name)
	if err != nil {
		return err
	}
	if o.cloud.secretName != "" {
		return fmt.Errorf("--api-key-secret is k8s only; on docker export the key (e.g. OPENAI_API_KEY) or use --api-key-env")
	}
	next := cur
	if o.repo == "" { // rollback
		p, err := compose.LoadPrevious(o.name)
		if err != nil {
			return err
		}
		next.Repo, next.File, next.Provider, next.BaseURL = p.Repo, p.File, p.Provider, p.BaseURL
	} else {
		next.Repo, next.File = o.repo, o.file
		if o.cloud.changed {
			next.Provider, next.BaseURL = o.cloud.provider, o.cloud.baseURL
			if o.cloud.isCloud() {
				key, _ := providerKey(o.cloud)
				if key == "" && o.cloud.needsKey() && !providerKeyStored(o.name, o.cloud.provider) {
					return missingKeyErr(o.cloud, false)
				}
				next.APIKey = key
			}
		}
	}
	if !next.IsCloud() {
		next.Provider, next.BaseURL, next.APIKey = "", "", ""
		if cur.IsCloud() {
			// Cloud → local: the gateway did DLP; don't silently drop it.
			next.DLPMode, next.DLPAction, next.DLPImage = compose.DLPModeEnforce, cur.DLPAction, ""
		}
	}
	from, to := stackLabel(cur), stackLabel(next)
	if from == to && cur.BaseURL == next.BaseURL {
		fmt.Fprintf(out, "Stack %s already runs %s; nothing to do.\n", o.name, to)
		return nil
	}
	fmt.Fprintf(out, "Stack:     bairav-%s (docker)\nFrom:      %s\nTo:        %s\n", o.name, from, to)
	fmt.Fprintln(out, "           (docker recreates the container: expect a short outage; port, DLP and settings are kept)")
	fmt.Fprintln(out)

	failure := compose.Up(ctx, next, out, cmd.ErrOrStderr())
	if failure == nil {
		failure = waitDockerHealthy(ctx, o.name, o.timeout, out)
	}
	if failure == nil && o.smoke {
		fmt.Fprintln(out, "Smoke test ...")
		failure = smokeHTTP(ctx, next.Endpoint(), o.prompt, out)
	}
	if failure == nil {
		// Record only after success, so a failed attempt never becomes the
		// "previous model" that `rollback` restores.
		prev := compose.PreviousModel{Repo: cur.Repo, File: cur.File, Provider: cur.Provider, BaseURL: cur.BaseURL}
		if err := compose.SavePrevious(o.name, prev); err != nil {
			return fmt.Errorf("upgraded, but could not record the previous model for rollback: %w", err)
		}
		fmt.Fprintf(out, "\n✓ bairav-%s now serves %s at %s\n  Undo with: bairav model rollback %s\n", o.name, to, next.Endpoint(), o.name)
		return nil
	}
	fmt.Fprintf(out, "\n✗ Upgrade failed: %v\n", failure)
	if !o.rollback {
		return fmt.Errorf("upgrade failed (left in place because of --no-rollback): %w", failure)
	}
	fmt.Fprintf(out, "Rolling back to %s ...\n", from)
	if err := compose.Up(ctx, cur, out, cmd.ErrOrStderr()); err != nil {
		return fmt.Errorf("upgrade failed (%v) AND rollback failed: %w", failure, err)
	}
	if err := waitDockerHealthy(ctx, o.name, o.timeout, out); err != nil {
		return fmt.Errorf("upgrade failed (%v); rollback started but not confirmed: %w", failure, err)
	}
	return fmt.Errorf("upgrade to %s failed and was rolled back; %s is serving again: %w", to, from, failure)
}

// stackLabel is modelRef.label for docker stacks.
func stackLabel(s compose.Spec) string {
	if s.IsCloud() {
		return s.Provider + ":" + s.Repo
	}
	return modelLabel(s.Repo, s.File)
}

// waitDockerHealthy waits for the stack's serving container to report
// "(healthy)" (the compose healthcheck curls llama-server's /health).
func waitDockerHealthy(ctx context.Context, name string, timeout time.Duration, out io.Writer) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		stacks, err := listDocker(ctx)
		if err != nil {
			return err
		}
		status := "not running"
		for _, s := range stacks {
			if s.Kind == compose.KindModel && s.Name == name && s.Role == compose.RoleServing {
				status = s.Status
			}
		}
		if strings.Contains(status, "(healthy)") {
			fmt.Fprintln(out, "  ✓ healthy")
			return nil
		}
		if strings.Contains(status, "(unhealthy)") || strings.HasPrefix(strings.ToLower(status), "exited") {
			return fmt.Errorf("serving container is %s", status)
		}
		if status != last {
			fmt.Fprintf(out, "  … %s\n", status)
			last = status
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the model to become healthy", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// smokeHTTP sends one chat completion to a host-reachable /v1 endpoint.
func smokeHTTP(ctx context.Context, endpoint, prompt string, out io.Writer) error {
	body, err := smokeBody(prompt)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("smoke prompt: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("smoke prompt: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return checkSmoke(raw, out)
}

// -----------------------------------------------------------------------------
// shared
// -----------------------------------------------------------------------------

func smokeBody(prompt string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 16,
	})
}

// checkSmoke accepts any OpenAI-shaped reply with at least one choice. The
// content itself isn't judged: small models answer "OK" loosely, and the
// point is "the new model loads and answers", not quality.
func checkSmoke(raw []byte, out io.Writer) error {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("smoke prompt: not an OpenAI-style reply: %s", truncate(string(raw), 200))
	}
	if len(r.Choices) == 0 {
		return fmt.Errorf("smoke prompt: reply had no choices: %s", truncate(string(raw), 200))
	}
	fmt.Fprintf(out, "  ✓ model answered: %q\n", truncate(strings.TrimSpace(r.Choices[0].Message.Content), 60))
	return nil
}

func modelLabel(repo, file string) string {
	if file == "" {
		return repo
	}
	return repo + " (" + file + ")"
}
