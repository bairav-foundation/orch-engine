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
	Short: "Check a model's answers against a YAML suite",
	Long: `Runs a suite of prompts against any OpenAI-compatible endpoint and checks
each reply: expected words, regex, JSON, length, latency, and data
protection (no PII in the reply, DLP redacted or blocked the prompt).

  bairav eval init > my-suite.yaml
  bairav eval run my-suite.yaml --endpoint http://localhost:8080/v1
  bairav eval run my-suite.yaml --endpoint http://localhost:8080/v1 --against http://localhost:8081/v1

Exits 1 when the score is below the suite's threshold, so it works in CI.
Use the same suite as an upgrade gate: bairav model upgrade NAME --to REPO --eval my-suite.yaml`,
}

var evalRunCmd = &cobra.Command{
	Use:   "run SUITE.yaml",
	Short: "Run a suite and print a pass/fail table",
	Args:  cobra.ExactArgs(1),
	RunE:  runEval,
}

var evalInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Print a starter suite",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := io.WriteString(cmd.OutOrStdout(), starterSuite)
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
	evalCmd.AddCommand(evalRunCmd, evalInitCmd)
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
	out := cmd.OutOrStdout()

	suite, err := eval.Load(args[0])
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
	for _, t := range targets {
		o := eval.Options{Target: t, Model: model, Detectors: dets}
		if !asJSON && len(targets) == 1 {
			fmt.Fprintf(out, "Suite %s: %d cases against %s\n\n", suite.Name, len(suite.Cases), t)
			o.OnCase = func(c eval.CaseResult) { printEvalCase(out, c, verbose) }
		} else if !asJSON {
			fmt.Fprintf(cmd.ErrOrStderr(), "running %s against %s ...\n", suite.Name, t)
		}
		reports = append(reports, eval.Run(ctx, suite, eval.HTTPPoster(t, key, client), o))
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
	} else if len(reports) > 1 {
		renderEvalCompare(out, reports[0], reports[1])
	} else {
		fmt.Fprintln(out)
		fmt.Fprintln(out, evalSummary(reports[0]))
	}
	for _, r := range reports {
		if !r.Pass {
			return fmt.Errorf("eval %s: %s scored %d/%d, below the %.0f%% threshold", r.Suite, r.Target, r.Passed, r.Total, r.Threshold*100)
		}
	}
	return nil
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
