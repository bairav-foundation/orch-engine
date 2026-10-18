// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bairav-foundation/orch-engine/cli/internal/compose"
	"github.com/bairav-foundation/orch-engine/cli/internal/hf"
	"github.com/bairav-foundation/orch-engine/cli/internal/k8sclient"
	"github.com/bairav-foundation/orch-engine/cli/internal/serve"
	"github.com/bairav-foundation/orch-engine/cli/internal/serve/llamaserver"
	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// modelCmd groups `bairav model <subcommand>` — pull, serve, list, delete, deploy, undeploy.
//
// Subcommands:
//
//	pull     — download a model from Hugging Face
//	list     — models in ~/.bairav/models
//	serve    — run one locally with llama-server
//	delete   — remove a cached model
//	deploy   — run it on Docker or Kubernetes
//	undeploy — stop it
var modelCmd = &cobra.Command{
	Use:   "model",
	Short: "Manage LLM deployments (pull, serve, list, delete, deploy, undeploy)",
	Long: `Deploy open-source models (Llama, Mistral, Qwen, etc.) onto a
Kubernetes cluster or local Docker. Models are pulled from Hugging Face,
auto-quantized by default, and exposed via an OpenAI-compatible endpoint.`,
}

func init() {
	modelCmd.AddCommand(modelPullCmd)
	modelCmd.AddCommand(modelServeCmd)
	modelCmd.AddCommand(modelDeployCmd)
	modelCmd.AddCommand(modelUndeployCmd)
	modelCmd.AddCommand(modelListCmd)
	modelCmd.AddCommand(modelDeleteCmd)

	modelPullCmd.Flags().StringP("file", "f", "",
		"specific file path inside the repo to pull (default: smallest GGUF)")
	modelPullCmd.Flags().Bool("all", false,
		"pull every file in the repo (warning: can be many GB)")
	modelPullCmd.Flags().String("revision", "main",
		"git revision (branch, tag, or commit) to pull from")

	modelServeCmd.Flags().StringP("file", "f", "",
		"specific file in the repo to serve (default: smallest cached/available GGUF)")
	modelServeCmd.Flags().String("revision", "main",
		"git revision used when auto-pulling")
	modelServeCmd.Flags().IntP("port", "p", 8080, "TCP port to bind")
	modelServeCmd.Flags().String("host", "127.0.0.1",
		"bind address — use 0.0.0.0 to expose on the LAN")
	modelServeCmd.Flags().Int("ctx-size", 0,
		"context window in tokens (0 = backend default)")

	modelListCmd.Flags().BoolP("verbose", "v", false,
		"show every file inside each cached repo (not just the primary GGUF)")

	modelDeleteCmd.Flags().BoolP("force", "f", false,
		"skip the confirmation prompt")

	modelDeployCmd.Flags().String("target", "",
		"where to deploy: docker (Compose, default), k8s (Kubernetes via Model CR). Aliases: compose=docker, kubernetes=k8s.")
	modelDeployCmd.Flags().StringP("name", "n", "",
		"name of the deployment (default: derived from <hf-repo>)")
	modelDeployCmd.Flags().String("namespace", "default",
		"Kubernetes namespace to deploy into (k8s target only)")
	modelDeployCmd.Flags().StringP("file", "f", "",
		"specific file in the repo to serve (default: smallest GGUF)")
	modelDeployCmd.Flags().Int("port", 8080,
		"host-side port for the model endpoint (docker target) or Service port (k8s target)")
	modelDeployCmd.Flags().Int("ctx-size", 0,
		"context window in tokens (0 = backend default)")
	modelDeployCmd.Flags().String("image", "",
		"override serving container image (default: the serving image matching this CLI version)")
	modelDeployCmd.Flags().Bool("wait", true,
		"poll until phase=Ready before returning (use --wait=false to return immediately, k8s target only)")
	modelDeployCmd.Flags().Duration("timeout", 5*time.Minute,
		"max time to wait for phase=Ready when --wait is set (k8s target only)")
	// DLP enforcement flags — currently docker-target-only. See
	// compose.DLPMode: --dlp is the friendly on/off bool that flips the
	// internal mode enum; the enum can grow with more values (e.g. "on"
	// = proxy alongside, not enforced) without changing the CLI.
	modelDeployCmd.Flags().Bool("dlp", false,
		"protect the model with a DLP proxy sidecar; the model becomes unreachable except through the proxy (docker target only)")
	modelDeployCmd.Flags().String("dlp-action", "redact",
		"DLP proxy default action when --dlp is set: redact | block | audit_only")
	modelDeployCmd.Flags().String("dlp-policy", "",
		"path to a YAML DLP policy file (mounted read-only into the DLP container; used only when --dlp is set)")

	modelDeployCmd.Flags().String("dlp-ref", "",
		"name of a DLPPolicy (same namespace) to enforce inside the Model Pod; the model is then reachable only through DLP. "+
			"Kept on later deploys unless you pass --dlp-ref \"\" (k8s target only)")

	modelDeployCmd.Flags().String("backend", "",
		"k8s target: inference engine, llama-server (default, CPU, GGUF) | vllm (experimental: GPU, safetensors repos). Kept on later deploys unless passed")
	modelDeployCmd.Flags().Int("gpus", 0,
		"k8s target, --backend vllm: GPUs per Pod (nvidia.com/gpu; default 1)")

	addCloudFlags(modelDeployCmd) // --provider openai|anthropic|openai-compatible

	modelUndeployCmd.Flags().String("target", "",
		"where the deployment lives: docker or k8s (default: docker)")
	modelUndeployCmd.Flags().String("namespace", "default",
		"namespace the Model lives in (k8s target only)")
	modelUndeployCmd.Flags().BoolP("force", "f", false,
		"skip the confirmation prompt")
	modelUndeployCmd.Flags().Bool("cascade-agents", false,
		"also delete any Agents (CRs on k8s, stacks on docker) that reference this Model. "+
			"Without this flag, dependent Agents remain (k8s: phase=WaitingForDeps; docker: running with a dead endpoint).")
}

// -------------------------------------------------------------------------
// model pull
// -------------------------------------------------------------------------

var modelPullCmd = &cobra.Command{
	Use:   "pull <hf-repo>",
	Short: "Pull a model from Hugging Face into the local cache",
	Long: `Download a model from the Hugging Face Hub into the local cache at
~/.bairav/models/<org>/<repo>/.

Default behaviour:
  bairav model pull Qwen/Qwen2.5-0.5B-Instruct-GGUF
    → pulls the smallest .gguf file in the repo (good first-time UX).

Override:
  --file <path>   pull a specific file by its path inside the repo.
  --all           pull every file (use with care for multi-GB repos).

Authentication is via HF_TOKEN env var if the repo is gated or private.
Public repos work anonymously.`,
	Args: cobra.ExactArgs(1),
	RunE: runModelPull,
}

func runModelPull(cmd *cobra.Command, args []string) error {
	repo := args[0]
	specificFile, _ := cmd.Flags().GetString("file")
	pullAll, _ := cmd.Flags().GetBool("all")
	revision, _ := cmd.Flags().GetString("revision")

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client := hf.NewClient()

	fmt.Fprintf(cmd.OutOrStdout(), "Listing files in %s @ %s ...\n", repo, revision)
	files, err := client.ListFiles(ctx, repo, revision)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("repo %s has no files", repo)
	}

	// Decide which files to pull.
	var targets []hf.RepoFile
	switch {
	case pullAll:
		targets = files
	case specificFile != "":
		for _, f := range files {
			if f.Path == specificFile {
				targets = []hf.RepoFile{f}
				break
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("file %q not found in repo %s", specificFile, repo)
		}
	default:
		// Smallest .gguf — best first-time UX for llama.cpp-style bundles.
		smallest := smallestGGUF(files)
		if smallest == nil {
			suggestions := searchGGUFSiblings(ctx, client, repo)
			return noGGUFInRepoError(repo, files, suggestions)
		}
		targets = []hf.RepoFile{*smallest}
	}

	for _, f := range targets {
		dst, err := hf.LocalPath(repo, f.Path)
		if err != nil {
			return err
		}
		url := client.BlobURL(repo, revision, f.Path)
		fmt.Fprintf(cmd.OutOrStdout(), "→ %s  (%s)\n", f.Path, hf.HumanBytes(f.Size))
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", dst)

		bar := newProgressBar(cmd.OutOrStdout(), f.Size)
		if err := client.Download(ctx, url, dst, bar.update); err != nil {
			bar.finish()
			return fmt.Errorf("download %s: %w", f.Path, err)
		}
		bar.finish()
	}

	fmt.Fprintf(cmd.OutOrStdout(), "\nDone. %d file(s) cached. Run `bairav model list` to see them.\n", len(targets))
	return nil
}

// smallestGGUF returns a pointer to the smallest *.gguf file in the list, or nil.
func smallestGGUF(files []hf.RepoFile) *hf.RepoFile {
	var best *hf.RepoFile
	for i := range files {
		f := files[i]
		if !strings.HasSuffix(strings.ToLower(f.Path), ".gguf") {
			continue
		}
		if best == nil || f.Size < best.Size {
			best = &files[i]
		}
	}
	return best
}

// -------------------------------------------------------------------------
// model serve
// -------------------------------------------------------------------------

var modelServeCmd = &cobra.Command{
	Use:   "serve <hf-repo>",
	Short: "Serve a model locally with an OpenAI-compatible endpoint",
	Long: `Start a local inference server for the given model on localhost:8080
with an OpenAI-compatible API at /v1/chat/completions.

If the model isn't cached yet, 'serve' auto-pulls it first (same logic as
'bairav model pull'). Backend defaults to llama-server (llama.cpp); future
versions add vllm and ollama as --backend choices.

Example:
  bairav model serve Qwen/Qwen2.5-0.5B-Instruct-GGUF
  curl localhost:8080/v1/chat/completions -d '{"model":"...","messages":[...]}'

Foreground process: Ctrl-C cleanly stops the backend.`,
	Args: cobra.ExactArgs(1),
	RunE: runModelServe,
}

func runModelServe(cmd *cobra.Command, args []string) error {
	repo := args[0]
	specificFile, _ := cmd.Flags().GetString("file")
	revision, _ := cmd.Flags().GetString("revision")
	port, _ := cmd.Flags().GetInt("port")
	host, _ := cmd.Flags().GetString("host")
	ctxSize, _ := cmd.Flags().GetInt("ctx-size")

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	out := cmd.OutOrStdout()

	// 1. Resolve which file to serve.
	modelFile, err := resolveServingFile(ctx, out, repo, revision, specificFile)
	if err != nil {
		return err
	}

	// 2. Pick + check backend availability before doing anything heavy.
	backend := llamaserver.New()
	if err := backend.Available(); err != nil {
		return fmt.Errorf(
			"%w\n\nInstall with:\n  brew install llama.cpp        # macOS\n"+
				"  see https://github.com/ggml-org/llama.cpp/releases  # Linux/Windows",
			err)
	}

	// 3. Spawn the backend.
	cfg := serve.Config{
		ModelPath:   modelFile,
		ModelName:   repo,
		Host:        host,
		Port:        port,
		ContextSize: ctxSize,
		Stdout:      cmd.ErrOrStderr(), // backend logs go to stderr; our UX to stdout
		Stderr:      cmd.ErrOrStderr(),
	}
	fmt.Fprintf(out, "Starting %s on %s:%d ...\n", backend.Name(), host, port)
	if err := backend.Start(ctx, cfg); err != nil {
		return fmt.Errorf("start backend: %w", err)
	}

	// 4. Wait for readiness.
	readyCtx, readyCancel := context.WithTimeout(ctx, 60*time.Second)
	defer readyCancel()
	fmt.Fprintf(out, "Waiting for %s to load %s ...\n",
		backend.Name(), filepath.Base(modelFile))
	if err := backend.WaitReady(readyCtx); err != nil {
		_ = backend.Stop(context.Background())
		return fmt.Errorf("backend not ready after 60s: %w", err)
	}

	// 4b. Record run-state so `bairav ps` (and future `status`) can find us.
	state := serve.State{
		Port:      port,
		Host:      host,
		Model:     repo,
		ModelFile: modelFile,
		Backend:   backend.Name(),
		PID:       backend.PID(),
		StartedAt: time.Now(),
	}
	if err := serve.WriteState(state); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: write run-state: %v\n", err)
	}
	defer func() { _ = serve.RemoveState(port) }()

	// 5. Tell the user what to do next.
	fmt.Fprintf(out, "\n✓ Ready: %s\n", backend.Endpoint())
	tier := serve.Classify(filepath.Base(modelFile), repo)
	if tier.Params > 0 {
		fmt.Fprintf(out, "  Model: ~%sB params · tier: %s (%s)\n",
			tier.FormatParams(), tier.Name, tier.UseCases)
		if upgrade := tier.SuggestedUpgrade(); upgrade != "" {
			fmt.Fprintf(out, "  Stuck? Try a larger one: bairav model serve %s\n", upgrade)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Try it:")
	fmt.Fprintf(out, "  curl %s/chat/completions \\\n", backend.Endpoint())
	fmt.Fprintf(out, "    -H 'Content-Type: application/json' \\\n")
	fmt.Fprintf(out, "    -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello!\"}]}'\n\n",
		repo)
	fmt.Fprintf(out, "Or interactively:\n  bairav chat %s\n\n", repo)
	fmt.Fprintln(out, "Press Ctrl-C to stop the server.")

	// 6. Block until interrupted, then shut down cleanly.
	<-ctx.Done()
	fmt.Fprintln(out, "\nShutting down ...")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := backend.Stop(stopCtx); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
	}
	return nil
}

// resolveServingFile picks the .gguf to serve, auto-pulling from HF if it's
// not already cached. Returns the absolute path of the file on disk.
//
// Distinguishes three cache states so error messages are useful:
//  1. Cache hit (a usable .gguf is on disk)        → serve it.
//  2. Repo cached but no .gguf among its files     → explicit error listing
//     what IS there; do NOT
//     fall through to pull
//     (HF likely has no GGUF
//     either, and silently
//     hitting the network
//     for a confusing second
//     error is worse UX).
//  3. Repo not cached at all                       → pull from HF.
func resolveServingFile(
	ctx context.Context, out io.Writer,
	repo, revision, specificFile string,
) (string, error) {
	cachedRepo := findCachedRepo(repo)
	client := hf.NewClient()

	// State 1: cache has what we need — use it.
	if cachedRepo != nil {
		if cached := findCachedFile(repo, specificFile); cached != "" {
			fmt.Fprintf(out, "Using cached: %s\n", cached)
			return cached, nil
		}
		// State 2: repo present, but no usable .gguf (or specificFile not found).
		// Surface sibling GGUF repos via HF search so the user doesn't context-switch.
		suggestions := searchGGUFSiblings(ctx, client, repo)
		return "", cachedButUnusableError(repo, specificFile, cachedRepo, suggestions)
	}

	// State 3: cache miss → pull from HF.
	fmt.Fprintf(out, "Model not cached — pulling from Hugging Face ...\n")
	files, err := client.ListFiles(ctx, repo, revision)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("repo %s has no files", repo)
	}

	var target hf.RepoFile
	if specificFile != "" {
		found := false
		for _, f := range files {
			if f.Path == specificFile {
				target = f
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("file %q not found in %s — check the path with `bairav model pull %s --all` first, or with HF's web UI", specificFile, repo, repo)
		}
	} else {
		smallest := smallestGGUF(files)
		if smallest == nil {
			suggestions := searchGGUFSiblings(ctx, client, repo)
			return "", noGGUFInRepoError(repo, files, suggestions)
		}
		target = *smallest
	}

	dst, err := hf.LocalPath(repo, target.Path)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "→ %s  (%s)\n", target.Path, hf.HumanBytes(target.Size))
	bar := newProgressBar(out, target.Size)
	if err := client.Download(ctx, client.BlobURL(repo, revision, target.Path), dst, bar.update); err != nil {
		bar.finish()
		return "", fmt.Errorf("download %s: %w", target.Path, err)
	}
	bar.finish()
	return dst, nil
}

// findCachedRepo returns a pointer to the cached entry for repo, or nil.
func findCachedRepo(repo string) *hf.CachedModel {
	cached, err := hf.ListCached()
	if err != nil {
		return nil
	}
	for i := range cached {
		if cached[i].Repo == repo {
			return &cached[i]
		}
	}
	return nil
}

// cachedButUnusableError builds the error for "repo is cached but llama-server
// can't load any of its files" — a common confusion when users pull a
// safetensors-only repo and then try to serve it. Suggestions, if non-empty,
// are sibling repos returned by HF search that DO contain GGUF.
func cachedButUnusableError(repo, specificFile string, m *hf.CachedModel, suggestions []string) error {
	header := fmt.Sprintf("%s is cached locally but contains no .gguf file (llama-server only loads GGUF).", repo)
	if specificFile != "" {
		header = fmt.Sprintf("%s is cached but %q is not present (or is not a .gguf).",
			repo, specificFile)
	}
	var lines []string
	lines = append(lines, header, "")
	lines = append(lines, "Cached files:")
	for _, f := range m.Files {
		marker := "  "
		if strings.HasSuffix(strings.ToLower(f.Path), ".gguf") {
			marker = "✓ "
		}
		lines = append(lines, fmt.Sprintf("  %s%s  (%s)",
			marker, f.Path, hf.HumanBytes(f.Size)))
	}
	lines = append(lines, "")
	if len(suggestions) > 0 {
		lines = append(lines, "GGUF-bearing repos on Hugging Face matching this name:")
		for _, s := range suggestions {
			lines = append(lines, fmt.Sprintf("  bairav model serve %s", s))
		}
		lines = append(lines, "")
	}
	lines = append(lines, "Other fixes:")
	lines = append(lines, "  - Pass --file <one-of-the-above> to force a specific cached file.")
	lines = append(lines, "  - Free the disk space: `bairav model delete "+repo+"` then pull a GGUF variant.")
	return fmt.Errorf("%s", strings.Join(lines, "\n"))
}

// noGGUFInRepoError builds the error for "Hugging Face listed this repo and it
// has files, but none are .gguf" — usually a safetensors-only checkpoint repo.
// suggestions are sibling repos returned by HF search that DO contain GGUF.
func noGGUFInRepoError(repo string, files []hf.RepoFile, suggestions []string) error {
	var lines []string
	lines = append(lines,
		fmt.Sprintf("no .gguf files in %s — llama-server requires GGUF format.", repo),
		"",
		"Files HuggingFace returned for this repo (sample):",
	)
	count := 0
	for _, f := range files {
		if count >= 8 {
			lines = append(lines, fmt.Sprintf("  ... and %d more", len(files)-count))
			break
		}
		lines = append(lines, fmt.Sprintf("  %s  (%s)", f.Path, hf.HumanBytes(f.Size)))
		count++
	}
	lines = append(lines, "")
	if len(suggestions) > 0 {
		lines = append(lines, "GGUF-bearing repos on Hugging Face matching this name:")
		for _, s := range suggestions {
			lines = append(lines, fmt.Sprintf("  bairav model serve %s", s))
		}
		lines = append(lines, "")
	}
	lines = append(lines,
		"Other fixes:",
		"  - Pass --file <name> to fetch a specific non-GGUF file (won't work with `serve`, but `pull` will fetch it).",
	)
	return fmt.Errorf("%s", strings.Join(lines, "\n"))
}

// searchGGUFSiblings queries HF for GGUF-bearing repos matching the model
// name in repo. Returns up to 5 repo IDs (excluding repo itself), sorted by
// HF's own ranking. Returns nil on any error — this is a UX hint, not
// load-bearing, so a slow/down HF mustn't make our error worse.
func searchGGUFSiblings(ctx context.Context, client *hf.Client, repo string) []string {
	hits, err := client.SearchModels(ctx, lastSegment(repo), true, 5)
	if err != nil {
		return nil
	}
	var ids []string
	for _, h := range hits {
		if h.ID == repo {
			continue
		}
		ids = append(ids, h.ID)
		if len(ids) >= 5 {
			break
		}
	}
	return ids
}

// lastSegment returns the part after the final "/" in a HF repo slug, e.g.
// "Qwen/Qwen3.6-27B" → "Qwen3.6-27B". Used for "try the *-GGUF sibling" hints.
func lastSegment(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// findCachedFile returns the absolute path to a cached file for repo, or "" if
// not found. If specificFile is empty, returns the smallest .gguf in the cache.
func findCachedFile(repo, specificFile string) string {
	cached, err := hf.ListCached()
	if err != nil {
		return ""
	}
	for _, m := range cached {
		if m.Repo != repo {
			continue
		}
		// Match a specific file if requested.
		if specificFile != "" {
			for _, f := range m.Files {
				if f.Path == specificFile {
					if abs, err := hf.LocalPath(repo, f.Path); err == nil {
						return abs
					}
				}
			}
			return ""
		}
		// Otherwise: smallest .gguf.
		var bestPath string
		var bestSize int64
		for _, f := range m.Files {
			if !strings.HasSuffix(strings.ToLower(f.Path), ".gguf") {
				continue
			}
			if bestPath == "" || f.Size < bestSize {
				bestPath = f.Path
				bestSize = f.Size
			}
		}
		if bestPath == "" {
			return ""
		}
		if abs, err := hf.LocalPath(repo, bestPath); err == nil {
			return abs
		}
	}
	return ""
}

// -------------------------------------------------------------------------
// model deploy
// -------------------------------------------------------------------------

var modelDeployCmd = &cobra.Command{
	Use:   "deploy <hf-repo>",
	Short: "Deploy a model on Docker Compose (default) or Kubernetes",
	Long: `Bring up an open-source LLM end-to-end. The default target is Docker
Compose — runs the same bairav/serving:laptop image as the K8s path, no
cluster required. Pass --target k8s to create a Model CR in the active
kubeconfig context instead; the BAIRAVS operator reconciles it into a
Deployment + Service.

Example:
  bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF                  # docker (default)
  bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s
  bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s --namespace bairav-models --timeout 10m
  bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s --wait=false`,
	Args: cobra.ExactArgs(1),
	RunE: runModelDeploy,
}

// resolveTarget normalises the --target flag value. Returns one of "docker"
// or "k8s", or an error for unknown values. Empty input → "docker" but the
// caller is expected to print the banner; we don't print here because some
// callers (tests, future automation) won't want stderr noise.
func resolveTarget(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "docker", "compose":
		return "docker", nil
	case "k8s", "kubernetes":
		return "k8s", nil
	default:
		return "", fmt.Errorf("unknown --target %q (use docker or k8s)", raw)
	}
}

func runModelDeploy(cmd *cobra.Command, args []string) error {
	repo := args[0]
	targetRaw, _ := cmd.Flags().GetString("target")
	target, err := resolveTarget(targetRaw)
	if err != nil {
		return err
	}
	// Print the banner when --target was unset so users always know what
	// the implicit choice was. They can suppress by passing --target docker
	// explicitly (same outcome, no banner).
	if targetRaw == "" {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"--target not specified; defaulting to docker. "+
				"Pass --target k8s to deploy to your active kubeconfig context.")
	}

	name, _ := cmd.Flags().GetString("name")
	file, _ := cmd.Flags().GetString("file")
	port, _ := cmd.Flags().GetInt("port")
	ctxSize, _ := cmd.Flags().GetInt("ctx-size")
	image, _ := cmd.Flags().GetString("image")

	if name == "" {
		name = k8sNameFromRepo(repo)
	}
	cloud, err := readCloudOpts(cmd)
	if err != nil {
		return err
	}
	if cloud.isCloud() && (file != "" || image != "" || ctxSize != 0) {
		return fmt.Errorf("--file / --image / --ctx-size are for local models; with --provider %s the provider runs the model", cloud.provider)
	}
	if flagChanged(cmd, "dlp-ref") && (target != "k8s" || cloud.isCloud()) {
		return fmt.Errorf("--dlp-ref is for local models on --target k8s (docker: use --dlp; cloud models always run behind the DLP gateway)")
	}
	backend, _ := cmd.Flags().GetString("backend")
	gpus, _ := cmd.Flags().GetInt("gpus")
	if err := checkBackendFlags(backend, gpus, target, file, cloud.isCloud()); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if cloud.isCloud() {
		dlpAction, _ := cmd.Flags().GetString("dlp-action")
		switch target {
		case "docker":
			return deployCloudToCompose(ctx, cmd, name, repo, port, cloud, dlpAction)
		case "k8s":
			namespace, _ := cmd.Flags().GetString("namespace")
			wait, _ := cmd.Flags().GetBool("wait")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			return deployToK8s(ctx, cmd, name, namespace, repo, file, image, port, ctxSize, wait, timeout,
				func(ctx context.Context, c ctrlclient.Client, m *orchv1alpha1.Model) error {
					if err := applyCloudK8s(ctx, c, namespace, name, cloud, &m.Spec, cmd.OutOrStdout()); err != nil {
						return err
					}
					if m.Spec.Cloud != nil && cmd.Flags().Changed("dlp-action") {
						m.Spec.Cloud.DLPAction = dlpAction
					}
					return nil
				})
		}
	}

	switch target {
	case "docker":
		dlpOn, _ := cmd.Flags().GetBool("dlp")
		dlpAction, _ := cmd.Flags().GetString("dlp-action")
		dlpPolicy, _ := cmd.Flags().GetString("dlp-policy")
		return deployToCompose(ctx, cmd, name, repo, file, image, port, ctxSize,
			dlpOn, dlpAction, dlpPolicy)
	case "k8s":
		namespace, _ := cmd.Flags().GetString("namespace")
		wait, _ := cmd.Flags().GetBool("wait")
		timeout, _ := cmd.Flags().GetDuration("timeout")
		dlpRef, _ := cmd.Flags().GetString("dlp-ref")
		mutate := func(_ context.Context, _ ctrlclient.Client, m *orchv1alpha1.Model) error {
			if dlpRef != "" {
				m.Spec.DLPRef = &orchv1alpha1.LocalObjectReference{Name: dlpRef}
			}
			applyBackend(&m.Spec, backend, gpus)
			return nil
		}
		return deployToK8s(ctx, cmd, name, namespace, repo, file, image, port, ctxSize, wait, timeout, mutate)
	}
	return fmt.Errorf("internal: unhandled target %q", target)
}

// deployToCompose materialises the model as a docker compose stack.
// When dlpOn is true the stack becomes two services — the model is
// unpublished on the host, DLP proxy is the only entry point.
func deployToCompose(ctx context.Context, cmd *cobra.Command, name, repo, file, image string, port, ctxSize int,
	dlpOn bool, dlpAction, dlpPolicy string) error {
	out := cmd.OutOrStdout()
	spec := compose.Spec{
		Name:     name,
		Repo:     repo,
		File:     file,
		Image:    image,
		HostPort: port,
		CtxSize:  ctxSize,
		HFToken:  os.Getenv("HF_TOKEN"),
	}
	if dlpOn {
		spec.DLPMode = compose.DLPModeEnforce
		spec.DLPAction = dlpAction
		if dlpPolicy != "" {
			abs, err := filepath.Abs(dlpPolicy)
			if err != nil {
				return fmt.Errorf("--dlp-policy: resolve absolute path: %w", err)
			}
			// Fail fast if the file is missing rather than let docker's
			// bind-mount surface a confusing error later.
			if _, err := os.Stat(abs); err != nil {
				return fmt.Errorf("--dlp-policy: %w", err)
			}
			spec.DLPPolicyHostPath = abs
		}
	}

	fmt.Fprintf(out, "Target:    docker (compose)\n")
	fmt.Fprintf(out, "Name:      %s\n", spec.Name)
	fmt.Fprintf(out, "Repo:      %s\n", spec.Repo)
	if file != "" {
		fmt.Fprintf(out, "File:      %s\n", spec.File)
	}
	if image != "" {
		fmt.Fprintf(out, "Image:     %s\n", spec.Image)
	}
	if dlpOn {
		fmt.Fprintf(out, "DLP:       enforce (proxy is the only entry point)\n")
		fmt.Fprintf(out, "  action:  %s (per-rule action overrides in policy still win)\n", spec.DLPAction)
		if spec.DLPPolicyHostPath != "" {
			fmt.Fprintf(out, "  policy:  %s\n", spec.DLPPolicyHostPath)
		}
	}
	fmt.Fprintf(out, "Endpoint:  %s\n", spec.Endpoint())
	if dir, err := compose.StackDir(spec.Name); err == nil {
		fmt.Fprintf(out, "StackDir:  %s\n", dir)
	}
	fmt.Fprintln(out)

	if err := compose.Up(ctx, spec, out, cmd.ErrOrStderr()); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n✓ Stack up. First boot pulls the GGUF inside the container — give it a minute.\n")
	fmt.Fprintf(out, "  Check status: bairav ps\n")
	fmt.Fprintf(out, "  Talk to it:   bairav chat --endpoint %s\n", spec.Endpoint())
	if dlpOn {
		fmt.Fprintf(out, "  Note:         the raw model port is bound to the compose network only.\n")
		fmt.Fprintf(out, "                Prompts reach the model only through the DLP proxy.\n")
	}
	return nil
}

// deployToK8s creates (or updates) a Model CR in the active kubeconfig
// context and optionally polls for phase=Ready.
// mutate, when non-nil, adjusts the Model before it's created/updated
// (cloud models: provider, gateway config, key Secret).
func deployToK8s(ctx context.Context, cmd *cobra.Command, name, namespace, repo, file, image string, port, ctxSize int, wait bool, timeout time.Duration,
	mutate func(context.Context, ctrlclient.Client, *orchv1alpha1.Model) error) error {
	out := cmd.OutOrStdout()
	c, err := k8sclient.New()
	if err != nil {
		return err
	}

	// Show what we're about to do so the user can Ctrl-C if context is wrong.
	fmt.Fprintf(out, "Target:    k8s\n")
	fmt.Fprintf(out, "Context:   %s\n", k8sclient.CurrentContext())
	fmt.Fprintf(out, "Namespace: %s\n", namespace)
	fmt.Fprintf(out, "Name:      %s\n", name)
	fmt.Fprintf(out, "Repo:      %s\n", repo)
	if image != "" {
		fmt.Fprintf(out, "Image:     %s\n", image)
	}
	fmt.Fprintln(out)

	// Ensure the target namespace exists. Create-if-missing is idempotent
	// and is what every kubectl-style tool does for the user — without it,
	// `deploy --namespace bairav-models` fails with "namespaces not found"
	// the first time a new namespace is used. We never delete namespaces
	// on the undeploy side (only the Model CR), so it's safe to autocreate.
	if err := ensureNamespace(ctx, c, namespace, out); err != nil {
		return err
	}

	desired := buildModelCR(name, namespace, repo, file, image, int32(port), int32(ctxSize))
	if mutate != nil {
		if err := mutate(ctx, c, desired); err != nil {
			return err
		}
	}

	var existing orchv1alpha1.Model
	key := types.NamespacedName{Name: name, Namespace: namespace}
	err = c.Get(ctx, key, &existing)
	switch {
	case apierrors.IsNotFound(err):
		fmt.Fprintf(out, "Creating Model %s/%s ...\n", namespace, name)
		if err := c.Create(ctx, desired); err != nil {
			return fmt.Errorf("create model: %w", err)
		}
		fmt.Fprintln(out, "✓ Created")
	case err == nil:
		fmt.Fprintf(out, "Updating existing Model %s/%s ...\n", namespace, name)
		// Never drop model-side DLP by accident: keep an existing dlpRef
		// unless the user explicitly passed --dlp-ref (possibly "").
		if desired.Spec.DLPRef == nil && existing.Spec.DLPRef != nil && !flagChanged(cmd, "dlp-ref") {
			desired.Spec.DLPRef = existing.Spec.DLPRef
			fmt.Fprintf(out, "Keeping DLPPolicy %s (pass --dlp-ref \"\" to remove it)\n", existing.Spec.DLPRef.Name)
		}
		// Same for the backend: a plain redeploy of a vLLM model stays on vLLM.
		if !flagChanged(cmd, "backend") && existing.Spec.Backend == orchv1alpha1.ModelBackendVLLM && !desired.Spec.IsCloud() {
			desired.Spec.Backend = existing.Spec.Backend
			if !flagChanged(cmd, "gpus") {
				desired.Spec.Resources = existing.Spec.Resources
			}
			fmt.Fprintln(out, "Keeping backend vllm (pass --backend llama-server to switch)")
		}
		existing.Spec = desired.Spec
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update model: %w", err)
		}
		fmt.Fprintln(out, "✓ Updated")
	default:
		return fmt.Errorf("get model: %w", err)
	}

	if !wait {
		fmt.Fprintln(out, "(--wait=false; not polling for Ready)")
		return nil
	}
	return pollUntilReady(ctx, c, key, timeout, out, cmd.ErrOrStderr())
}

// -------------------------------------------------------------------------
// model undeploy
// -------------------------------------------------------------------------

var modelUndeployCmd = &cobra.Command{
	Use:   "undeploy <name>",
	Short: "Tear down a deployed model (Docker Compose or Kubernetes)",
	Long: `Remove a deployment. With --target docker (default), runs
'docker compose down -v' on the stack and removes ~/.bairav/compose/<name>/.
With --target k8s, deletes the Model CR; the operator cascades the Deployment
and Service.

Example:
  bairav model undeploy qwen2-5-0-5b-instruct-gguf
  bairav model undeploy qwen-tiny --target k8s --namespace bairav-models`,
	Args: cobra.ExactArgs(1),
	RunE: runModelUndeploy,
}

func runModelUndeploy(cmd *cobra.Command, args []string) error {
	name := args[0]
	targetRaw, _ := cmd.Flags().GetString("target")
	target, err := resolveTarget(targetRaw)
	if err != nil {
		return err
	}
	if targetRaw == "" {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"--target not specified; defaulting to docker. "+
				"Pass --target k8s to tear down a Model CR.")
	}
	force, _ := cmd.Flags().GetBool("force")

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cascade, _ := cmd.Flags().GetBool("cascade-agents")
	switch target {
	case "docker":
		// lists (and with --cascade-agents deletes) docker Agent
		// stacks that reference this Model, same semantics as k8s.
		return undeployDockerWithDependents(ctx, cmd, compose.KindModel, compose.LabelRefModelPrefix,
			name, name, force, cascade)
	case "k8s":
		namespace, _ := cmd.Flags().GetString("namespace")
		return undeployFromK8s(ctx, cmd, name, namespace, force, cascade)
	}
	return fmt.Errorf("internal: unhandled target %q", target)
}

func undeployFromK8s(ctx context.Context, cmd *cobra.Command, name, namespace string, force, cascade bool) error {
	out := cmd.OutOrStdout()
	c, err := k8sclient.New()
	if err != nil {
		return err
	}

	// Fetch the Model. If it's already gone, we still allow the command
	// to proceed WHEN --cascade-agents is set — the common workflow is
	// "I undeployed the Model, now I realise I want the orphaned Agents
	// gone too." Without --cascade-agents, a missing Model is a plain
	// error (idempotent-no-op would silently mask typos).
	var m orchv1alpha1.Model
	modelExists := true
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := c.Get(ctx, key, &m); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get model: %w", err)
		}
		modelExists = false
		if !cascade {
			return fmt.Errorf("model %s/%s not found in current context "+
				"(if the Model is already gone and you want to clean up orphaned Agents, "+
				"re-run with --cascade-agents)", namespace, name)
		}
	}

	if modelExists {
		fmt.Fprintf(out, "About to delete Model %s/%s (repo: %s, phase: %s).\n",
			namespace, name, m.Spec.Repo, m.Status.Phase)
	} else {
		fmt.Fprintf(out, "Model %s/%s already gone. Looking for orphaned Agents to cascade-delete ...\n",
			namespace, name)
	}

	// Before deleting, list Agents that reference this Model by NAME so
	// we can find dependents even when the Model CR is gone. This is
	// exactly what makes the "clean up after the fact" flow work.
	dependents, err := listDependentAgents(ctx, c, "Model", name, namespace)
	if err != nil {
		return fmt.Errorf("check dependent agents: %w", err)
	}
	if modelExists {
		printDependentAgents(out, "Model", dependents, cascade)
	} else {
		// Model is already gone; cascade is on. Show what we found.
		if len(dependents) == 0 {
			fmt.Fprintln(out, "No dependent Agents remain. Nothing to do.")
			return nil
		}
		fmt.Fprintf(out, "%d Agent(s) still reference this Model:\n", len(dependents))
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

	// Cascade first, Model second — so if cascade partial-fails, the
	// Model still exists and we haven't stranded partial state.
	if cascade && len(dependents) > 0 {
		if err := cascadeDeleteAgents(ctx, c, out, dependents); err != nil {
			return fmt.Errorf("cascade agents: %w", err)
		}
	}

	if modelExists {
		if err := c.Delete(ctx, &m); err != nil {
			return fmt.Errorf("delete model: %w", err)
		}
		fmt.Fprintf(out, "✓ Deleted Model %s/%s\n", namespace, name)
	}
	return nil
}

// -------------------------------------------------------------------------
// deploy / undeploy helpers
// -------------------------------------------------------------------------

// ensureNamespace creates the given namespace if it doesn't already exist.
// kubectl autocreates namespaces transparently for the user; we do the same.
// On undeploy we never delete namespaces (only the Model CR), so this is
// safe: a stale empty namespace is fine, removing the user's other
// resources by surprise is not.
func ensureNamespace(ctx context.Context, c ctrlclient.Client, name string, out io.Writer) error {
	var ns corev1.Namespace
	err := c.Get(ctx, types.NamespacedName{Name: name}, &ns)
	switch {
	case err == nil:
		// already exists, nothing to do
		return nil
	case apierrors.IsNotFound(err):
		fmt.Fprintf(out, "Namespace %q does not exist — creating it.\n", name)
		ns = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "bairav",
				},
			},
		}
		if err := c.Create(ctx, &ns); err != nil {
			// Possible if a parallel deploy created it between our Get and
			// Create — that's still "exists", so treat AlreadyExists as ok.
			if apierrors.IsAlreadyExists(err) {
				return nil
			}
			return fmt.Errorf("create namespace %q: %w", name, err)
		}
		return nil
	default:
		return fmt.Errorf("get namespace %q: %w", name, err)
	}
}

// checkBackendFlags validates --backend / --gpus against the rest of the
// deploy flags.
func checkBackendFlags(backend string, gpus int, target, file string, cloud bool) error {
	switch backend {
	case "", orchv1alpha1.ModelBackendLlamaServer:
		if gpus != 0 {
			return fmt.Errorf("--gpus is for --backend vllm")
		}
		return nil
	case orchv1alpha1.ModelBackendVLLM:
	default:
		return fmt.Errorf("unknown --backend %q (use llama-server or vllm)", backend)
	}
	switch {
	case target != "k8s":
		return fmt.Errorf("--backend vllm is k8s only for now (it needs a GPU node); on docker run vLLM yourself and use --provider openai-compatible")
	case cloud:
		return fmt.Errorf("--backend is for local models; with --provider the provider runs the model")
	case file != "":
		return fmt.Errorf("--file picks a GGUF for llama-server; vLLM serves the whole repo (use a safetensors repo such as Qwen/Qwen2.5-1.5B-Instruct)")
	case gpus < 0:
		return fmt.Errorf("--gpus must be 1 or more")
	}
	return nil
}

// applyBackend sets the backend (and GPU count) on a Model spec. An empty
// backend leaves the spec's default.
func applyBackend(spec *orchv1alpha1.ModelSpec, backend string, gpus int) {
	if backend == "" {
		return
	}
	spec.Backend = backend
	if backend == orchv1alpha1.ModelBackendVLLM && gpus > 0 {
		q := resource.MustParse(fmt.Sprintf("%d", gpus))
		spec.Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": q}
	}
}

// buildModelCR turns CLI args into a v1alpha1 Model spec.
func buildModelCR(name, namespace, repo, file, image string, port, ctxSize int32) *orchv1alpha1.Model {
	m := &orchv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "bairav",
			},
		},
		Spec: orchv1alpha1.ModelSpec{
			Repo:        repo,
			File:        file,
			Revision:    "main",
			Backend:     orchv1alpha1.ModelBackendLlamaServer,
			ContextSize: ctxSize,
			Service: orchv1alpha1.ModelServiceSpec{
				Type: corev1.ServiceTypeClusterIP,
				Port: port,
			},
		},
	}
	if image != "" {
		m.Spec.Image = image
	}
	return m
}

// pollUntilReady tails the Model's status, printing phase transitions until
// it reaches Ready, Failed, the context is cancelled, or timeout elapses.
func pollUntilReady(ctx context.Context, c ctrlclient.Client,
	key types.NamespacedName, timeout time.Duration,
	out io.Writer, errOut io.Writer) error {
	deadline := time.Now().Add(timeout)
	fmt.Fprintf(out, "Waiting for Ready (timeout %s) ...\n", timeout)

	lastPhase := orchv1alpha1.ModelPhase("")
	lastDLPMsg := ""
	start := time.Now()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(errOut, "")
			fmt.Fprintln(errOut, "Timed out waiting for Ready.")
			fmt.Fprintf(errOut, "Check status: kubectl describe model -n %s %s\n",
				key.Namespace, key.Name)
			return fmt.Errorf("timeout after %s; last phase: %s", timeout, lastPhase)
		}

		var m orchv1alpha1.Model
		if err := c.Get(ctx, key, &m); err != nil {
			return fmt.Errorf("poll model: %w", err)
		}
		if m.Status.Phase != lastPhase {
			fmt.Fprintf(out, "  Phase: %-10s %s\n",
				m.Status.Phase, time.Since(start).Round(time.Second))
			lastPhase = m.Status.Phase
		}
		if cond := meta.FindStatusCondition(m.Status.Conditions, "DLPEnforced"); cond != nil && cond.Status == metav1.ConditionFalse && cond.Message != lastDLPMsg {
			fmt.Fprintf(errOut, "  Waiting: %s\n", cond.Message)
			lastDLPMsg = cond.Message
		}

		switch m.Status.Phase {
		case orchv1alpha1.ModelPhaseReady:
			fmt.Fprintln(out, "")
			fmt.Fprintf(out, "✓ Ready: %s\n", m.Status.Endpoint)
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "Talk to it from your laptop:")
			fmt.Fprintf(out, "  kubectl port-forward -n %s svc/%s %d:%d\n",
				key.Namespace, key.Name, m.Spec.Service.Port, m.Spec.Service.Port)
			fmt.Fprintf(out, "  bairav chat --endpoint http://localhost:%d/v1\n",
				m.Spec.Service.Port)
			return nil
		case orchv1alpha1.ModelPhaseFailed:
			return fmt.Errorf("deployment failed; check `kubectl describe model -n %s %s`",
				key.Namespace, key.Name)
		}

		time.Sleep(2 * time.Second)
	}
}

// k8sNameRe matches characters that are NOT allowed in a K8s DNS-1123 name.
var k8sNameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// suffixNoiseTokens are trailing HF repo-name tokens that carry useful
// information for a model registry but are pure noise once you're typing the
// resulting name into a kubectl command or a DNS bar. Stripping them keeps
// the cluster DNS like "qwen2-5-0-5b.default.svc" instead of "qwen2-5-0-5b-
// instruct-gguf.default.svc" — same model, much more readable.
//
// We only strip *trailing* tokens (we never strip from the middle) and we
// stop at the first token that isn't noise, so something like "foo-bar-chat"
// becomes "foo-bar" but "chat-foo-bar" is left alone — "chat" in the middle
// might be part of the actual model name.
var suffixNoiseTokens = map[string]bool{
	"gguf":     true, // file format
	"instruct": true, // tuning style
	"chat":     true, // tuning style
	"it":       true, // Gemma's instruct tag
	"base":     true, // un-tuned variant marker
	"tuned":    true,
	"ft":       true, // fine-tuned
	"awq":      true, // quantization
	"gptq":     true,
	"exl2":     true,
	"exllama":  true,
}

// suffixVersionRe matches "v1", "v0.2", "v1.0", etc. — trailing version
// tags that don't help disambiguate a deployment.
var suffixVersionRe = regexp.MustCompile(`^v\d+(\.\d+)*$`)

// stripNoisySuffixes removes trailing GGUF/Instruct/Chat/version tokens
// from a lowercased, hyphen-separated repo name. Operates on raw tokens
// (which may still contain ".") and re-joins with "-"; final DNS-1123
// sanitisation happens in the caller.
func stripNoisySuffixes(lower string) string {
	tokens := strings.Split(lower, "-")
	for len(tokens) > 1 {
		last := tokens[len(tokens)-1]
		if suffixNoiseTokens[last] || suffixVersionRe.MatchString(last) {
			tokens = tokens[:len(tokens)-1]
			continue
		}
		break
	}
	return strings.Join(tokens, "-")
}

// k8sNameFromRepo turns an HF repo slug into a readable K8s resource name.
// Pipeline: take the last segment after "/", lowercase, strip trailing
// noise tokens (GGUF, Instruct, Chat, versions), replace any disallowed
// char with "-", collapse runs of "-", trim, cap at 63 chars (DNS-1123 limit).
//
//	Qwen/Qwen2.5-0.5B-Instruct-GGUF       -> qwen2-5-0-5b
//	bartowski/Mistral-7B-Instruct-v0.2-GGUF -> mistral-7b
//	TinyLlama/TinyLlama-1.1B-Chat-v1.0    -> tinyllama-1-1b
//
// The user can always override with --name; this is just the default.
func k8sNameFromRepo(repo string) string {
	last := lastSegment(repo)
	s := strings.ToLower(last)
	s = stripNoisySuffixes(s)
	s = k8sNameRe.ReplaceAllString(s, "-")
	// Collapse runs of "-" into one.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = s[:63]
		s = strings.TrimRight(s, "-")
	}
	if s == "" {
		s = "bairav-model"
	}
	return s
}

// -------------------------------------------------------------------------
// model delete
// -------------------------------------------------------------------------

var modelDeleteCmd = &cobra.Command{
	Use:   "delete <hf-repo>",
	Short: "Delete a cached model from local disk",
	Long: `Remove a cached model directory from ~/.bairav/models/<repo>/ and free
the disk space.

Cannot be undone (re-run 'bairav model pull <repo>' to re-download).

Example:
  bairav model delete Qwen/Qwen2.5-0.5B-Instruct-GGUF
  bairav model delete -f Qwen/Qwen2.5-0.5B-Instruct-GGUF   # skip prompt`,
	Args: cobra.ExactArgs(1),
	RunE: runModelDelete,
}

func runModelDelete(cmd *cobra.Command, args []string) error {
	repo := args[0]
	force, _ := cmd.Flags().GetBool("force")
	out := cmd.OutOrStdout()

	cachedRepo := findCachedRepo(repo)
	if cachedRepo == nil {
		return fmt.Errorf("repo %s is not cached — run `bairav model list` to see what is", repo)
	}

	fmt.Fprintf(out, "About to delete %s — %d file(s), %s.\n",
		cachedRepo.Repo, len(cachedRepo.Files), hf.HumanBytes(cachedRepo.TotalSize))

	if !force {
		fmt.Fprint(out, "Type 'yes' to confirm: ")
		var answer string
		_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
		if strings.TrimSpace(strings.ToLower(answer)) != "yes" {
			fmt.Fprintln(out, "Aborted (no changes).")
			return nil
		}
	}

	// Resolve repo dir from any cached file's path — guaranteed to exist
	// since cachedRepo is non-nil.
	repoDir, err := hf.LocalPath(repo, "")
	if err != nil {
		return fmt.Errorf("resolve cache path: %w", err)
	}
	// Strip the trailing separator that LocalPath() appends for empty file.
	repoDir = strings.TrimRight(repoDir, string(os.PathSeparator))

	if err := os.RemoveAll(repoDir); err != nil {
		return fmt.Errorf("delete %s: %w", repoDir, err)
	}

	// Try to clean up the now-empty <org>/ parent (only if it has nothing else).
	parent := filepath.Dir(repoDir)
	if entries, err := os.ReadDir(parent); err == nil && len(entries) == 0 {
		_ = os.Remove(parent)
	}

	fmt.Fprintf(out, "✓ Deleted %s — freed %s\n", repo, hf.HumanBytes(cachedRepo.TotalSize))
	return nil
}

// -------------------------------------------------------------------------
// model list
// -------------------------------------------------------------------------

var modelListCmd = &cobra.Command{
	Use:   "list",
	Short: "List locally cached models",
	RunE:  runModelList,
}

func runModelList(cmd *cobra.Command, _ []string) error {
	verbose, _ := cmd.Flags().GetBool("verbose")

	cached, err := hf.ListCached()
	if err != nil {
		return err
	}
	if len(cached) == 0 {
		root, _ := hf.CacheDir()
		fmt.Fprintf(cmd.OutOrStdout(),
			"No models cached yet at %s.\nTry: bairav model pull Qwen/Qwen2.5-0.5B-Instruct-GGUF\n",
			root)
		return nil
	}

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	defer tw.Flush()
	sort.Slice(cached, func(i, j int) bool { return cached[i].Repo < cached[j].Repo })

	if verbose {
		// One row per file — full transparency on what's actually on disk.
		fmt.Fprintln(tw, "REPO\tFILE\tSIZE")
		for _, m := range cached {
			for _, f := range m.Files {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", m.Repo, f.Path, hf.HumanBytes(f.Size))
			}
		}
		return nil
	}

	// Default: compact view, but always show what `serve` would pick (or
	// flag clearly that nothing GGUF is available).
	fmt.Fprintln(tw, "REPO\tPRIMARY (served by default)\tFILES\tSIZE")
	for _, m := range cached {
		primary := pickPrimaryGGUF(m.Files)
		if primary == "" {
			primary = "⚠ no .gguf — `serve` won't work; use -v to see files"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n",
			m.Repo, primary, len(m.Files), hf.HumanBytes(m.TotalSize))
	}
	return nil
}

// pickPrimaryGGUF returns the basename of the smallest .gguf in files, or "".
// Same selection rule `serve` uses — keeps `list` and `serve` consistent.
func pickPrimaryGGUF(files []hf.CachedFile) string {
	var best string
	var bestSize int64
	for _, f := range files {
		if !strings.HasSuffix(strings.ToLower(f.Path), ".gguf") {
			continue
		}
		if best == "" || f.Size < bestSize {
			best = f.Path
			bestSize = f.Size
		}
	}
	return best
}

// -------------------------------------------------------------------------
// progress bar
// -------------------------------------------------------------------------

// progressBar prints a single-line progress update to w, throttled to every
// ~100ms so we don't flood the terminal on fast disks. Falls back to byte
// counts only if total is unknown (-1).
type progressBar struct {
	w     io.Writer
	total int64
	last  time.Time
}

func newProgressBar(w io.Writer, total int64) *progressBar {
	return &progressBar{w: w, total: total}
}

func (p *progressBar) update(downloaded, total int64) {
	if total <= 0 {
		total = p.total
	}
	now := time.Now()
	if now.Sub(p.last) < 100*time.Millisecond && downloaded < total {
		return
	}
	p.last = now
	if total <= 0 {
		fmt.Fprintf(p.w, "\r  %s downloaded", hf.HumanBytes(downloaded))
		return
	}
	pct := float64(downloaded) / float64(total) * 100
	fmt.Fprintf(p.w, "\r  %3.0f%%  %s / %s",
		pct, hf.HumanBytes(downloaded), hf.HumanBytes(total))
}

func (p *progressBar) finish() {
	fmt.Fprintln(p.w)
}

// flagChanged reports whether a flag exists on cmd and was set explicitly.
// Safe for commands that don't define the flag.
func flagChanged(cmd *cobra.Command, name string) bool {
	f := cmd.Flags().Lookup(name)
	return f != nil && f.Changed
}
