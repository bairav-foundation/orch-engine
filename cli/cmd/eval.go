// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
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

	"github.com/bairav-foundation/orch-engine/cli/internal/dlp"
	"github.com/bairav-foundation/orch-engine/cli/internal/dlp/packs"
	"github.com/bairav-foundation/orch-engine/cli/internal/eval"
)

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Check a model's answers with built-in or YAML suites",
	Long: `Runs suites of prompts against any OpenAI-compatible endpoint and checks
each reply: expected words, regex, JSON, length, latency, and data
protection (no PII in the reply, DLP redacted or blocked the prompt).

Built-in suites need no YAML (bairav eval list shows them):

  bairav eval run --builtin basics,json --endpoint http://localhost:8080/v1

Your own suites, or a built-in one copied and changed:

  bairav eval init --from basics > my-suite.yaml
  bairav eval run my-suite.yaml --endpoint http://localhost:8080/v1
  bairav eval run my-suite.yaml --endpoint http://localhost:8080/v1 --against http://localhost:8081/v1

Exits 1 when any suite scores below its threshold, so it works in CI
(--junit and --markdown write reports for CI). Use a suite as an upgrade
gate: bairav model upgrade NAME --to REPO --eval builtin:basics`,
}

var evalRunCmd = &cobra.Command{
	Use:   "run [SUITE.yaml ...]",
	Short: "Run suites and print a pass/fail table",
	Example: `  bairav eval run --builtin basics
  bairav eval run --builtin basics,json,privacy my-suite.yaml --junit eval.xml
  bairav eval run --builtin dlp --endpoint http://localhost:8081/v1   # the DLP proxy`,
	RunE: runEval,
}

var evalListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the built-in suites",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "%-10s %-6s %s\n", "NAME", "CASES", "CHECKS")
		for _, n := range eval.BuiltinNames() {
			s, err := eval.LoadBuiltin(n)
			if err != nil {
				return err
			}
			desc := s.Description
			if s.NeedsDLP {
				desc += " (run against a DLP proxy)"
			}
			fmt.Fprintf(out, "%-10s %-6d %s\n", n, len(s.Cases), desc)
		}
		fmt.Fprintln(out, "\nRun: bairav eval run --builtin NAME[,NAME]   Copy to change: bairav eval init --from NAME")
		return nil
	},
}

var evalInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Print a starter suite, or a built-in one to copy and change",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		from, _ := cmd.Flags().GetString("from")
		if from == "" {
			_, err := io.WriteString(cmd.OutOrStdout(), starterSuite)
			return err
		}
		b, err := eval.BuiltinYAML(from)
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(b)
		return err
	},
}

func init() {
	f := evalRunCmd.Flags()
	f.String("endpoint", "http://localhost:8080/v1", "OpenAI-compatible API base URL")
	f.String("against", "", "second endpoint to run the same suite on, shown side by side")
	f.String("model", "", "request \"model\" field (overrides the suite's)")
	f.StringSlice("pack", []string{"global-safe"}, "DLP packs for no_pii checks (global-safe, india, uk; none = built-ins only)")
	f.StringSlice("policy", nil, "extra DLP policy file(s) for no_pii checks")
	f.Bool("json", false, "print the report(s) as JSON")
	f.BoolP("verbose", "v", false, "print every reply")
	f.StringSlice("builtin", nil, "built-in suites to run (see bairav eval list), e.g. basics,json")
	f.String("junit", "", "also write a JUnit XML report to this file (for CI test views)")
	f.String("markdown", "", "also write a markdown summary to this file (e.g. $GITHUB_STEP_SUMMARY)")
	evalInitCmd.Flags().String("from", "", "print this built-in suite instead of the starter (see bairav eval list)")
	evalCmd.AddCommand(evalRunCmd, evalListCmd, evalInitCmd)
	rootCmd.AddCommand(evalCmd)
}

// evalAPIKey reads the key from the environment only, so it never lands in
// shell history or `ps`.
func evalAPIKey() string {
	for _, n := range []string{"BAIRAV_EVAL_API_KEY", "OPENAI_API_KEY"} {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// evalDetectors is the detector set for no_pii: built-ins + packs + policy
// files, the same layering as `bairav dlp test`.
func evalDetectors(packNames, policies []string) ([]dlp.Detector, error) {
	dets := dlp.DefaultDetectors()
	if names := normalisePackList(packNames); len(names) > 0 {
		pd, _, err := packs.LoadMany(names)
		if err != nil {
			return nil, err
		}
		dets = append(dets, pd...)
	}
	if len(policies) > 0 {
		extra, _, err := dlp.LoadPolicyFiles(policies)
		if err != nil {
			return nil, err
		}
		dets = append(dets, extra...)
	}
	return dets, nil
}

func runEval(cmd *cobra.Command, args []string) error {
	endpoint, _ := cmd.Flags().GetString("endpoint")
	against, _ := cmd.Flags().GetString("against")
	model, _ := cmd.Flags().GetString("model")
	packNames, _ := cmd.Flags().GetStringSlice("pack")
	policies, _ := cmd.Flags().GetStringSlice("policy")
	asJSON, _ := cmd.Flags().GetBool("json")
	verbose, _ := cmd.Flags().GetBool("verbose")
	builtins, _ := cmd.Flags().GetStringSlice("builtin")
	junitPath, _ := cmd.Flags().GetString("junit")
	mdPath, _ := cmd.Flags().GetString("markdown")
	out := cmd.OutOrStdout()

	suites, err := loadEvalSuites(builtins, args)
	if err != nil {
		return err
	}
	dets, err := evalDetectors(packNames, policies)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	targets := []string{endpoint}
	if against != "" {
		targets = append(targets, against)
	}
	client := &http.Client{Timeout: 10 * time.Minute} // per-case timeouts come from the suite
	key := evalAPIKey()
	var reports []*eval.Report
	for si, suite := range suites {
		var pair []*eval.Report
		for _, t := range targets {
			o := eval.Options{Target: t, Model: model, Detectors: dets}
			if !asJSON && len(targets) == 1 {
				if si > 0 {
					fmt.Fprintln(out)
				}
				fmt.Fprintf(out, "Suite %s: %d cases against %s\n\n", suite.Name, len(suite.Cases), t)
				o.OnCase = func(c eval.CaseResult) { printEvalCase(out, c, verbose) }
			} else if !asJSON {
				fmt.Fprintf(cmd.ErrOrStderr(), "running %s against %s ...\n", suite.Name, t)
			}
			r := eval.Run(ctx, suite, eval.HTTPPoster(t, key, client), o)
			pair = append(pair, r)
			if !asJSON && len(targets) == 1 {
				fmt.Fprintln(out)
				fmt.Fprintln(out, evalSummary(r))
				if !r.Pass && suite.NeedsDLP {
					fmt.Fprintln(out, "  (this suite checks a DLP proxy: point --endpoint at the proxy, not the bare model)")
				}
			}
		}
		if !asJSON && len(pair) > 1 {
			renderEvalCompare(out, pair[0], pair[1])
		}
		reports = append(reports, pair...)
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		var v any = reports[0]
		if len(reports) > 1 {
			v = reports
		}
		if err := enc.Encode(v); err != nil {
			return err
		}
	}
	if junitPath != "" {
		if err := writeEvalReport(junitPath, reports, eval.WriteJUnit); err != nil {
			return fmt.Errorf("--junit: %w", err)
		}
	}
	if mdPath != "" {
		if err := writeEvalReport(mdPath, reports, eval.WriteMarkdown); err != nil {
			return fmt.Errorf("--markdown: %w", err)
		}
	}
	var failed []string
	for _, r := range reports {
		if !r.Pass {
			failed = append(failed, fmt.Sprintf("%s on %s scored %d/%d (needs %.0f%%)", r.Suite, r.Target, r.Passed, r.Total, r.Threshold*100))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("eval below threshold: %s", strings.Join(failed, "; "))
	}
	return nil
}

// loadEvalSuites resolves --builtin names and suite files, in that order.
func loadEvalSuites(builtins, files []string) ([]*eval.Suite, error) {
	var suites []*eval.Suite
	for _, n := range builtins {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		s, err := eval.LoadBuiltin(n)
		if err != nil {
			return nil, err
		}
		suites = append(suites, s)
	}
	for _, f := range files {
		s, err := eval.Resolve(f)
		if err != nil {
			return nil, err
		}
		suites = append(suites, s)
	}
	if len(suites) == 0 {
		return nil, fmt.Errorf("nothing to run: pass a suite file or --builtin NAME (bairav eval list shows the built-in suites)")
	}
	return suites, nil
}

// writeEvalReport writes a report file; it appends when the file is
// $GITHUB_STEP_SUMMARY, which other steps write to as well.
func writeEvalReport(path string, reports []*eval.Report, write func(io.Writer, []*eval.Report) error) error {
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if path == os.Getenv("GITHUB_STEP_SUMMARY") {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		return err
	}
	if err := write(f, reports); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func printEvalCase(w io.Writer, c eval.CaseResult, verbose bool) {
	mark := "✓"
	if !c.Pass {
		mark = "✗"
	}
	fmt.Fprintf(w, "  %s %-28s %d/%d  %6d ms", mark, eval.Clip(c.Name, 28), c.Passed, c.Repeat, c.P50MS)
	if !c.Pass {
		fmt.Fprintf(w, "  %s", strings.Join(firstFailures(c), "; "))
	}
	fmt.Fprintln(w)
	if verbose {
		for _, a := range c.Attempts {
			fmt.Fprintf(w, "      → %s\n", eval.Clip(a.Reply, 100))
		}
	}
}

// firstFailures returns the failed checks of the first failing attempt.
func firstFailures(c eval.CaseResult) []string {
	for _, a := range c.Attempts {
		if !a.Passed() {
			return a.Failures
		}
	}
	return nil
}

func evalSummary(r *eval.Report) string {
	verdict := "PASS"
	if !r.Pass {
		verdict = "FAIL"
	}
	return fmt.Sprintf("Score %d/%d (%.0f%%), needs %.0f%%: %s   (%.1fs)", r.Passed, r.Total, r.Score*100, r.Threshold*100, verdict, r.Seconds)
}

func renderEvalCompare(w io.Writer, a, b *eval.Report) {
	fmt.Fprintf(w, "\nSuite %s\n  A = %s\n  B = %s\n\n", a.Suite, a.Target, b.Target)
	fmt.Fprintf(w, "  %-28s %-14s %-14s\n", "CASE", "A", "B")
	cell := func(c eval.CaseResult) string {
		m := "✓"
		if !c.Pass {
			m = "✗"
		}
		return fmt.Sprintf("%s %d ms", m, c.P50MS)
	}
	for i := range a.Cases {
		ca, cb := a.Cases[i], b.Cases[i]
		note := ""
		if ca.Pass != cb.Pass {
			note = "  ← differs"
		}
		fmt.Fprintf(w, "  %-28s %-14s %-14s%s\n", eval.Clip(ca.Name, 28), cell(ca), cell(cb), note)
	}
	fmt.Fprintf(w, "\n  A: %s\n  B: %s\n", evalSummary(a), evalSummary(b))
}

//nolint:gosec // G101: AKIAIOSFODNN7EXAMPLE below is AWS's published example key, used as a DLP test string
const starterSuite = `# bairav eval suite. Run: bairav eval run this.yaml --endpoint http://localhost:8080/v1
name: starter
threshold: 1          # share of cases that must pass (0..1)
defaults:
  temperature: 0      # always sent, so runs are repeatable
  max_tokens: 128
cases:
  - name: follows-instructions
    prompt: Reply with exactly the word OK.
    expect:
      regex: ['(?i)^\W*ok\W*$']

  - name: knows-basics
    prompt: What is the capital of Tamil Nadu? One word.
    expect:
      contains: [chennai]
      max_chars: 40

  - name: json-output
    prompt: 'Return a JSON object with keys "city" and "state" for Chennai. JSON only.'
    expect:
      json: true
      contains: ['"city"']

  - name: no-pii-in-reply
    prompt: Make up a short customer note. Do not include any email or phone number.
    expect:
      no_pii: true

  # With a DLP proxy in front of the model (bairav dlp serve, or a Model
  # with dlpRef), check that it actually rewrites the prompt:
  # - name: dlp-redacts-keys
  #   prompt: 'Repeat this back: AKIAIOSFODNN7EXAMPLE'
  #   expect:
  #     redacted: [AWS_ACCESS_KEY]
`

// evalGate runs a suite against a freshly upgraded model and fails the
// upgrade (so it rolls back) when the suite doesn't pass.
func evalGate(ctx context.Context, s *eval.Suite, post eval.Poster, target string, out io.Writer) error {
	fmt.Fprintf(out, "Eval gate: %s (%d cases) ...\n", s.Name, len(s.Cases))
	dets, err := evalDetectors([]string{"global-safe"}, nil)
	if err != nil {
		return err
	}
	r := eval.Run(ctx, s, post, eval.Options{Target: target, Detectors: dets,
		OnCase: func(c eval.CaseResult) { printEvalCase(out, c, false) }})
	fmt.Fprintln(out, "  "+evalSummary(r))
	if !r.Pass {
		return fmt.Errorf("eval gate %s: %d/%d cases passed, needs %.0f%%", s.Name, r.Passed, r.Total, r.Threshold*100)
	}
	return nil
}
