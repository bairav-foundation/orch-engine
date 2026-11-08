// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

// `bairav model fit REPO`: which file of a model runs on my GPU, Mac or
// CPU, without doing the math. Also behind `bairav model discover --memory`.

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/bairav-foundation/orch-engine/cli/internal/fit"
	"github.com/bairav-foundation/orch-engine/cli/internal/hf"
	"github.com/bairav-foundation/orch-engine/cli/internal/k8sclient"
	"github.com/bairav-foundation/orch-engine/cli/internal/serve"
)

var modelFitCmd = &cobra.Command{
	Use:   "fit REPO",
	Short: "Which file of a model runs on your GPU, Mac or CPU",
	Long: `Lists the GGUF files in a Hugging Face repo with the memory each needs
(the file, the context window and runtime overhead), checks them against
your machine and recommends the best one that fits.

Your machine is detected (NVIDIA GPU via nvidia-smi, Apple Silicon unified
memory, or CPU and RAM). Use --memory to size for another machine, or
--memory cluster for the biggest GPU node in the current Kubernetes context.

Estimates have headroom but are estimates: leave room for other apps.`,
	Example: `  bairav model fit Qwen/Qwen2.5-7B-Instruct-GGUF
  bairav model fit bartowski/Qwen2.5-32B-Instruct-GGUF --memory 24GB
  bairav model fit Qwen/Qwen2.5-7B-Instruct-GGUF --memory cluster --ctx 32768`,
	Args: cobra.ExactArgs(1),
	RunE: runModelFit,
}

func init() {
	addMemoryFlags(modelFitCmd, "auto")
	modelCmd.AddCommand(modelFitCmd)
}

// addMemoryFlags adds --memory and --ctx. def "" leaves sizing off.
func addMemoryFlags(c *cobra.Command, def string) {
	c.Flags().String("memory", def, "size for: auto (this machine) | cluster (biggest GPU node) | a size like 24GB")
	c.Flags().Int("ctx", fit.DefaultContext, "context window to size for, in tokens")
}

// resolveDevice turns --memory into a device.
func resolveDevice(ctx context.Context, spec string) (fit.Device, error) {
	switch strings.ToLower(strings.TrimSpace(spec)) {
	case "", "auto":
		return fit.Detect(ctx), nil
	case "cluster":
		return clusterDevice(ctx)
	}
	return fit.Manual(spec)
}

// clusterDevice reads the nodes of the current Kubernetes context: the
// node with the most GPU memory (NVIDIA GPU Feature Discovery labels),
// else the node with the most allocatable memory for CPU serving.
func clusterDevice(ctx context.Context) (fit.Device, error) {
	cs, err := k8sclient.NewClientset()
	if err != nil {
		return fit.Device{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fit.Device{}, fmt.Errorf("list nodes in %s: %w", k8sclient.CurrentContext(), err)
	}
	var gpu, cpu fit.Device
	for _, n := range nodes.Items {
		if mib, err := strconv.ParseInt(n.Labels["nvidia.com/gpu.memory"], 10, 64); err == nil && mib > 0 {
			b := mib << 20
			if b > gpu.Total {
				count, _ := strconv.Atoi(n.Labels["nvidia.com/gpu.count"])
				gpu = fit.Device{Kind: "cluster", Name: fmt.Sprintf("%s on node %s", or(n.Labels["nvidia.com/gpu.product"], "NVIDIA GPU"), n.Name),
					Total: b, Usable: b - (1 << 29), Count: max(count, 1)}
			}
			continue
		}
		if m, ok := n.Status.Allocatable["memory"]; ok && m.Value() > cpu.Total {
			cpu = fit.Device{Kind: "cluster", Name: fmt.Sprintf("node %s, CPU only (%s allocatable)", n.Name, fit.GB(m.Value())),
				Total: m.Value(), Usable: m.Value() * 3 / 4, Count: 1}
		}
	}
	switch {
	case gpu.Total > 0:
		return gpu, nil
	case cpu.Total > 0:
		return cpu, nil
	}
	return fit.Device{}, fmt.Errorf("no nodes with memory found in %s", k8sclient.CurrentContext())
}

func runModelFit(cmd *cobra.Command, args []string) error {
	repo := args[0]
	spec, _ := cmd.Flags().GetString("memory")
	ctxTokens, _ := cmd.Flags().GetInt("ctx")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	dev, err := resolveDevice(ctx, spec)
	if err != nil {
		return err
	}
	files, err := hf.NewClient().ListFiles(ctx, repo, "")
	if err != nil {
		return fmt.Errorf("%w\n(fit needs internet access to huggingface.co)", err)
	}
	paths := make([]string, len(files))
	sizes := make([]int64, len(files))
	for i, f := range files {
		paths[i], sizes[i] = f.Path, f.Size
	}
	params := int64(serve.ParseParamSize(repo) * 1e9)
	renderFit(cmd.OutOrStdout(), repo, dev, fit.Files(paths, sizes, params, dev.Usable, ctxTokens), params, ctxTokens, hasSplit(paths))
	return nil
}

func hasSplit(paths []string) bool {
	for _, p := range paths {
		if strings.Contains(strings.ToLower(p), "-of-0") && strings.HasSuffix(strings.ToLower(p), ".gguf") {
			return true
		}
	}
	return false
}

func fitMark(v fit.Verdict) string {
	switch v {
	case fit.Fits:
		return "✓ fits"
	case fit.Tight:
		return "~ tight"
	case fit.TooBig:
		return "✗ too big"
	}
	return "?"
}

func quantQuality(q string) string {
	for _, k := range fit.Quants {
		if k.Name == q {
			return k.Quality
		}
	}
	return ""
}

func renderFit(out io.Writer, repo string, dev fit.Device, files []fit.File, params int64, ctxTokens int, split bool) {
	fmt.Fprintf(out, "Sizing for: %s\n", dev.Describe())
	size := ""
	if params > 0 {
		size = " (" + hf.HumanParams(params) + ")"
	}
	fmt.Fprintf(out, "Model:      %s%s, context %d tokens\n\n", repo, size, ctxTokens)
	if len(files) == 0 {
		fmt.Fprintln(out, "No single-file GGUF in this repo, so bairav can't serve it directly.")
		if split {
			fmt.Fprintln(out, "(It has GGUFs split into parts; bairav serves one file.)")
		}
		fmt.Fprintln(out, "Try `bairav model discover` for runnable versions, or search for a -GGUF repo of the same model.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  FILE\tQUANT\tFILE SIZE\tNEEDS\tFIT")
	for _, f := range files {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", f.Path, or(f.Quant, "-"), fit.GB(f.Size), fit.GB(f.Need), fitMark(f.Verdict))
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
	if best := fit.Best(files); best != nil {
		q := best.Quant
		if qq := quantQuality(q); qq != "" {
			q += ", " + qq + " quality"
		}
		fmt.Fprintf(out, "Best for you: %s (%s, needs about %s)\n", best.Path, q, fit.GB(best.Need))
		fmt.Fprintf(out, "  bairav model deploy %s --file %s\n", repo, best.Path)
	} else {
		smallest := files[len(files)-1]
		fmt.Fprintf(out, "Nothing here fits %s: the smallest file needs about %s.\n", fit.GB(dev.Usable), fit.GB(smallest.Need))
		fmt.Fprintln(out, "Try a smaller model (bairav model discover --memory auto) or a shorter context (--ctx 4096).")
	}
	if split {
		fmt.Fprintln(out, "Files split into parts (-00001-of-0000N) aren't listed: bairav serves one file.")
	}
	fmt.Fprintln(out, "NEEDS = file + context window + runtime overhead. Estimates; leave room for other apps.")
}
