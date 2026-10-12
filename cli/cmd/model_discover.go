// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

// `bairav model discover`: the top open-weight models right now, and
// whether bairav can run each one today. New sovereign / regional models
// (Kolibri, Laguna, Solar Open, ...) land on Hugging Face every week; most
// people never hear of them. This makes them one command away.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/bairav-foundation/orch-engine/cli/internal/hf"
)

var modelDiscoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "Top open-weight models on Hugging Face right now, and how to run them",
	Long: `List the top open-weight models (default: 10 trending text models) from
Hugging Face, first-party releases only: fine-tunes, re-uploads and
"uncensored" variants are filtered out, and each family is shown once.

For each model: release age, size, license, and the GGUF repo bairav can
pull and serve (llama.cpp). "no GGUF yet" means only safetensors/vLLM
weights exist so far; community GGUFs usually follow within days.

Examples:
  bairav model discover
  bairav model discover --sort new --limit 20
  bairav model discover --task embedding
  bairav model discover --all            # raw HF ranking, no filtering`,
	Args: cobra.NoArgs,
	RunE: runModelDiscover,
}

func init() {
	modelDiscoverCmd.Flags().String("task", "text", "text | vision | embedding | image | speech")
	modelDiscoverCmd.Flags().String("sort", "trending", "trending | downloads | likes | new")
	modelDiscoverCmd.Flags().Int("limit", 10, "how many models to show")
	modelDiscoverCmd.Flags().Bool("all", false, "show the raw Hugging Face ranking (no first-party filter)")
	modelDiscoverCmd.Flags().Bool("json", false, "machine-readable output")
	modelCmd.AddCommand(modelDiscoverCmd)
}

func runModelDiscover(cmd *cobra.Command, _ []string) error {
	var o hf.DiscoverOptions
	o.Task, _ = cmd.Flags().GetString("task")
	o.Sort, _ = cmd.Flags().GetString("sort")
	o.Limit, _ = cmd.Flags().GetInt("limit")
	o.All, _ = cmd.Flags().GetBool("all")
	asJSON, _ := cmd.Flags().GetBool("json")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	models, err := hf.NewClient().Discover(ctx, o)
	if err != nil {
		return fmt.Errorf("%w\n(discover needs internet access to huggingface.co)", err)
	}
	out := cmd.OutOrStdout()
	if asJSON {
		return writeDiscoverJSON(out, models)
	}
	renderDiscover(out, models, o, time.Now())
	return nil
}

func renderDiscover(out io.Writer, models []hf.DiscoveredModel, o hf.DiscoverOptions, now time.Time) {
	scope := "first-party releases"
	if o.All {
		scope = "raw ranking"
	}
	fmt.Fprintf(out, "Top %d %s open-weight models on Hugging Face, by %s (%s)\n\n",
		len(models), orDefault(o.Task, "text"), orDefault(o.Sort, "trending"), scope)
	if len(models) == 0 {
		fmt.Fprintln(out, "Nothing matched. Try --all or a different --task.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tMODEL\tRELEASED\tSIZE\tLICENSE\tLIKES\tRUN WITH BAIRAV")
	firstPull, pullParams := "", int64(0)
	for i, m := range models {
		fmt.Fprintf(tw, "%d\t%s\t%s ago\t%s\t%s\t%d\t%s\n", i+1, m.ID, humanAge(now, m.Created),
			sizeLabel(m.Params), or(m.License(), "-"), m.Likes, runLabel(m, orDefault(o.Task, "text")))
		// Suggest the smallest runnable one: friendliest first deploy.
		if m.GGUF != "" && hf.Servable(orDefault(o.Task, "text")) && (firstPull == "" || (m.Params > 0 && (pullParams == 0 || m.Params < pullParams))) {
			firstPull, pullParams = m.GGUF, m.Params
		}
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
	if firstPull != "" {
		fmt.Fprintf(out, "Smallest runnable:  bairav model deploy %s --name my-model\n", firstPull)
	}
	fmt.Fprintln(out, "SIZE is total parameters; ~Q4 is the rough 4-bit GGUF download (≈ RAM needed).")
	fmt.Fprintln(out, "Check each model's license before production use.")
}

func sizeLabel(params int64) string {
	if params <= 0 {
		return "?"
	}
	return fmt.Sprintf("%s (~%s Q4)", hf.HumanParams(params), shortBytes(hf.Q4Bytes(params)))
}

// shortBytes is HumanBytes without the decimal noise for big numbers.
func shortBytes(n int64) string {
	const gib = 1 << 30
	if n >= 10*gib {
		return fmt.Sprintf("%d GiB", n/gib)
	}
	return hf.HumanBytes(n)
}

func runLabel(m hf.DiscoveredModel, task string) string {
	switch {
	case !hf.Servable(task):
		return "not servable yet (" + task + ")"
	case m.GGUF == "":
		return "no GGUF yet"
	case m.GGUF == m.ID:
		return "✓ pull this repo"
	default:
		return "✓ " + m.GGUF
	}
}

func writeDiscoverJSON(out io.Writer, models []hf.DiscoveredModel) error {
	type row struct {
		ID        string `json:"id"`
		Released  string `json:"released"`
		Params    int64  `json:"params,omitempty"`
		License   string `json:"license,omitempty"`
		Likes     int    `json:"likes"`
		Downloads int64  `json:"downloads"`
		GGUF      string `json:"gguf,omitempty"`
	}
	rows := make([]row, 0, len(models))
	for _, m := range models {
		rows = append(rows, row{m.ID, m.Created.Format(time.RFC3339), m.Params, m.License(), m.Likes, m.Downloads, m.GGUF})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
