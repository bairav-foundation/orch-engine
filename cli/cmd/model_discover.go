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
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/bairav-foundation/orch-engine/cli/internal/catalog"
	"github.com/bairav-foundation/orch-engine/cli/internal/fit"
	"github.com/bairav-foundation/orch-engine/cli/internal/hf"
)

var modelDiscoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "Open-weight models worth running, grouped by what they're best for",
	Long: `Without flags, shows the curated catalog: first-party open-weight models
grouped by what they're good at (on a laptop, general chat, most capable,
coding, reasoning, Indian languages, vision, embeddings), runnable ones
first. It's rebuilt every 12 hours from Hugging Face plus an editorial list,
and cached locally for 12 hours.

--live (or --task / --sort / --all) shows the live Hugging Face ranking
instead: first-party releases only, each family once.

"no GGUF yet" means only safetensors weights exist so far; community GGUFs
usually follow within days.

Examples:
  bairav model discover                    # curated, all groups
  bairav model discover --for coding       # one group
  bairav model discover --live --sort new  # what was released this week
  bairav model discover --task embedding
  bairav model discover --all              # raw HF ranking, no filtering`,
	Args: cobra.NoArgs,
	RunE: runModelDiscover,
}

func init() {
	modelDiscoverCmd.Flags().String("task", "text", "text | vision | embedding | image | speech")
	modelDiscoverCmd.Flags().String("sort", "trending", "trending | downloads | likes | new")
	modelDiscoverCmd.Flags().Int("limit", 10, "how many models to show")
	modelDiscoverCmd.Flags().Bool("all", false, "show the raw Hugging Face ranking (no first-party filter)")
	modelDiscoverCmd.Flags().Bool("json", false, "machine-readable output")
	modelDiscoverCmd.Flags().Bool("live", false, "live Hugging Face ranking instead of the curated catalog")
	modelDiscoverCmd.Flags().String("for", "", "curated group: "+strings.Join(catalog.Keys(), " | "))
	// Used by the catalog workflow, not by people.
	modelDiscoverCmd.Flags().String("build-catalog", "", "build the curated catalog from live data and write it to this path")
	modelDiscoverCmd.Flags().String("curation", "catalog/curated.yaml", "editorial pins/blocks for --build-catalog")
	_ = modelDiscoverCmd.Flags().MarkHidden("build-catalog")
	_ = modelDiscoverCmd.Flags().MarkHidden("curation")
	addMemoryFlags(modelDiscoverCmd, "")
	modelCmd.AddCommand(modelDiscoverCmd)
}

// sizing adds a FIT column to discover when --memory is set.
type sizing struct {
	dev     fit.Device
	ctx     int
	best    string // biggest model that fits so far
	bestP   int64
	bestRun string
}

func newSizing(cmd *cobra.Command) (*sizing, error) {
	spec, _ := cmd.Flags().GetString("memory")
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dev, err := resolveDevice(ctx, spec)
	if err != nil {
		return nil, err
	}
	n, _ := cmd.Flags().GetInt("ctx")
	return &sizing{dev: dev, ctx: n}, nil
}

// label is the FIT cell for a model of params parameters, and it records
// the biggest runnable model that fits for the summary line.
func (z *sizing) label(id, run string, params int64) string {
	if params <= 0 {
		return "size unknown"
	}
	p := fit.Recommend(params, z.dev.Usable, z.ctx)
	switch p.Verdict {
	case fit.Fits:
		if run != "" && params > z.bestP {
			z.best, z.bestP, z.bestRun = id, params, run
		}
		return fmt.Sprintf("✓ %s · %s", p.Quant.Name, fit.GB(p.Need))
	case fit.Tight:
		return fmt.Sprintf("~ %s tight · %s", p.Quant.Name, fit.GB(p.Need))
	}
	return "✗ needs " + fit.GB(fit.MinNeed(params, z.ctx)) + "+"
}

func (z *sizing) header(out io.Writer) {
	fmt.Fprintf(out, "Sizing for: %s, context %d tokens\n", z.dev.Describe(), z.ctx)
}

func (z *sizing) footer(out io.Writer) {
	if z.best == "" {
		fmt.Fprintf(out, "Nothing runnable here fits %s; try a shorter context (--ctx 4096).\n", fit.GB(z.dev.Usable))
		return
	}
	fmt.Fprintf(out, "Biggest that fits: %s. Pick the file:  bairav model fit %s --memory %s\n", z.best, z.bestRun, memoryArg(z.dev))
}

// memoryArg is the --memory value that reproduces this sizing.
func memoryArg(d fit.Device) string {
	switch d.Kind {
	case "cluster":
		return "cluster"
	case "manual":
		return strings.ReplaceAll(fit.GB(d.Usable), " ", "")
	}
	return "auto"
}

func runModelDiscover(cmd *cobra.Command, _ []string) error {
	if path, _ := cmd.Flags().GetString("build-catalog"); path != "" {
		return buildCatalog(cmd, path)
	}
	sz, err := newSizing(cmd)
	if err != nil {
		return err
	}
	live, _ := cmd.Flags().GetBool("live")
	if !live && !flagChanged(cmd, "task") && !flagChanged(cmd, "sort") && !flagChanged(cmd, "all") {
		done, err := runCuratedDiscover(cmd, sz)
		if done || err != nil {
			return err
		}
		// Catalog unavailable and nothing cached: fall through to live.
	}
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
	renderDiscover(out, models, o, time.Now(), sz)
	return nil
}

func renderDiscover(out io.Writer, models []hf.DiscoveredModel, o hf.DiscoverOptions, now time.Time, sz *sizing) {
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
	if sz != nil {
		sz.header(out)
		fmt.Fprintln(out)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	head := "#\tMODEL\tRELEASED\tSIZE\tLICENSE\tLIKES\tRUN WITH BAIRAV"
	if sz != nil {
		head += "\tFIT"
	}
	fmt.Fprintln(tw, head)
	firstPull, pullParams := "", int64(0)
	for i, m := range models {
		fitCell := ""
		if sz != nil {
			fitCell = "\t" + sz.label(m.ID, m.GGUF, m.Params)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s ago\t%s\t%s\t%d\t%s%s\n", i+1, m.ID, humanAge(now, m.Created),
			sizeLabel(m.Params), or(m.License(), "-"), m.Likes, runLabel(m, orDefault(o.Task, "text")), fitCell)
		// Suggest the smallest runnable one: friendliest first deploy.
		if m.GGUF != "" && hf.Servable(orDefault(o.Task, "text")) && (firstPull == "" || (m.Params > 0 && (pullParams == 0 || m.Params < pullParams))) {
			firstPull, pullParams = m.GGUF, m.Params
		}
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
	if sz != nil {
		sz.footer(out)
	} else if firstPull != "" {
		fmt.Fprintf(out, "Smallest runnable:  bairav model deploy %s --name my-model\n", firstPull)
		fmt.Fprintln(out, "Which fits your GPU or Mac? Add --memory auto (or --memory 24GB).")
	}
	fmt.Fprintln(out, "SIZE is total parameters; ~Q4 is the rough 4-bit GGUF download (≈ RAM needed).")
	fmt.Fprintln(out, "Check each model's license before production use.")
}

func sizeLabel(params int64) string {
	if params <= 0 {
		return "-" // GGUF-only repos don't publish a parameter count
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

// catalogURL is where the curated catalog is fetched from; override for
// mirrors or air-gapped installs with BAIRAV_CATALOG_URL.
func catalogURL() string {
	if u := os.Getenv("BAIRAV_CATALOG_URL"); u != "" {
		return u
	}
	return catalog.DefaultURL
}

// runCuratedDiscover prints the curated catalog. done=false means it
// couldn't be fetched (nor found in the cache) and the caller should fall
// back to the live ranking.
func runCuratedDiscover(cmd *cobra.Command, sz *sizing) (bool, error) {
	only, _ := cmd.Flags().GetString("for")
	asJSON, _ := cmd.Flags().GetBool("json")
	cachePath, _ := catalog.CachePath()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, stale, err := catalog.Fetch(ctx, &http.Client{}, catalogURL(), cachePath, time.Now())
	if err != nil {
		if only != "" {
			return true, fmt.Errorf("curated catalog unavailable (%v); try --live", err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Curated catalog unavailable (%v); showing the live Hugging Face ranking.\n\n", err)
		return false, nil
	}
	if only != "" {
		cat, ok := c.Find(only)
		if !ok {
			return true, fmt.Errorf("unknown group %q (use %s)", only, strings.Join(catalog.Keys(), ", "))
		}
		c.Categories = []catalog.Category{cat}
	}
	out := cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return true, enc.Encode(c)
	}
	renderCatalog(out, c, stale, time.Now(), sz)
	return true, nil
}

func renderCatalog(out io.Writer, c catalog.Catalog, stale bool, now time.Time, sz *sizing) {
	age := humanAge(now, c.Generated)
	note := ""
	if stale {
		note = " (couldn't refresh; showing the cached copy)"
	}
	fmt.Fprintf(out, "Open-weight models worth running, updated %s ago%s\n", age, note)
	if sz != nil {
		sz.header(out)
	}
	smallest := ""
	for _, cat := range c.Categories {
		fmt.Fprintf(out, "\n%s  ·  %s\n", strings.ToUpper(cat.Title), cat.Why)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, e := range cat.Models {
			run := "no GGUF yet"
			if e.GGUF != "" {
				run = "✓ " + e.GGUF
			}
			name := e.ID
			if e.Pinned {
				name += " ★"
			}
			fitCell := ""
			if sz != nil {
				fitCell = "\t" + sz.label(e.ID, e.GGUF, e.Params)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s%s\n", name, sizeLabel(e.Params), or(e.License, "-"), run, fitCell)
			if e.Note != "" {
				fmt.Fprintf(tw, "    %s\t\t\t\n", e.Note)
			}
			if cat.Key == "laptop" && smallest == "" && e.GGUF != "" {
				smallest = e.GGUF
			}
		}
		_ = tw.Flush()
	}
	fmt.Fprintln(out)
	if sz != nil {
		sz.footer(out)
	} else if smallest != "" {
		fmt.Fprintf(out, "Try one:  bairav model deploy %s --name my-model\n", smallest)
		fmt.Fprintln(out, "Which fits your GPU or Mac? Add --memory auto (or --memory 24GB).")
	}
	fmt.Fprintln(out, "★ = picked by us. One group: --for coding. Live ranking: --live. Check licenses before production use.")
}

// buildCatalog is what the 12-hourly workflow runs.
func buildCatalog(cmd *cobra.Command, path string) error {
	curPath, _ := cmd.Flags().GetString("curation")
	cur, err := catalog.LoadCuration(curPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c, err := catalog.Build(ctx, hf.NewClient(), cur, time.Now())
	if err != nil {
		return err
	}
	if err := catalog.Write(path, c); err != nil {
		return err
	}
	n := 0
	for _, cat := range c.Categories {
		n += len(cat.Models)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: %d groups, %d models\n", path, len(c.Categories), n)
	return nil
}
