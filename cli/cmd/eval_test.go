// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bairav-foundation/orch-engine/cli/internal/eval"
)

func TestStarterSuiteParses(t *testing.T) {
	s, err := eval.Parse([]byte(starterSuite), "starter.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Cases) != 4 || s.ThresholdOrDefault() != 1 {
		t.Fatalf("got %d cases, threshold %v", len(s.Cases), s.ThresholdOrDefault())
	}
}

func TestRenderEvalCompare(t *testing.T) {
	a := &eval.Report{Suite: "s", Target: "A", Passed: 1, Total: 1, Score: 1, Threshold: 1, Pass: true,
		Cases: []eval.CaseResult{{Name: "c", Pass: true, Passed: 1, Repeat: 1, P50MS: 10}}}
	b := &eval.Report{Suite: "s", Target: "B", Total: 1, Threshold: 1,
		Cases: []eval.CaseResult{{Name: "c", Repeat: 1, P50MS: 20}}}
	var buf bytes.Buffer
	renderEvalCompare(&buf, a, b)
	if !strings.Contains(buf.String(), "← differs") || !strings.Contains(buf.String(), "FAIL") {
		t.Fatalf("output:\n%s", buf.String())
	}
}

func TestLoadEvalSuites(t *testing.T) {
	s, err := loadEvalSuites([]string{"basics", " json", ""}, []string{"builtin:privacy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 3 || s[0].Name != "basics" || s[1].Name != "json" || s[2].Name != "privacy" {
		t.Fatalf("got %d suites", len(s))
	}
	if _, err := loadEvalSuites(nil, nil); err == nil || !strings.Contains(err.Error(), "eval list") {
		t.Fatalf("empty run should point at eval list, got %v", err)
	}
	if _, err := loadEvalSuites([]string{"nope"}, nil); err == nil {
		t.Fatal("unknown built-in should fail")
	}
}
