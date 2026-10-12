// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validPolicyYAML = `
version: v1
rules:
  - label: CUST_ID
    pattern: 'CUST-\d{7}'
    action: redact
  - label: INTERNAL_TOKEN
    pattern: 'bkey_[A-Za-z0-9]{16,}'
    action: block
  - label: PROJECT_CODE
    pattern: 'PROJ-[A-Z]{3}-\d{4}'
    # no action → inherits proxy default
`

func TestParsePolicy_ValidLoadsAllRules(t *testing.T) {
	dets, p, err := ParsePolicy([]byte(validPolicyYAML), "test.yaml")
	if err != nil {
		t.Fatalf("ParsePolicy: unexpected error: %v", err)
	}
	if len(dets) != 3 {
		t.Fatalf("expected 3 detectors, got %d", len(dets))
	}
	if p.Version != "v1" {
		t.Errorf("Version = %q, want v1", p.Version)
	}
	labels := SortedRuleLabels(p)
	want := []string{"CUST_ID", "INTERNAL_TOKEN", "PROJECT_CODE"}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("labels[%d] = %q, want %q", i, labels[i], want[i])
		}
	}
}

func TestParsePolicy_CustomRuleActuallyDetects(t *testing.T) {
	dets, _, err := ParsePolicy([]byte(validPolicyYAML), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Compose with the built-ins; run Scan over a mixed input.
	all := append([]Detector{}, dets...)
	all = append(all, DefaultDetectors()...)

	text := "customer CUST-4477291 hit rate limit; internal token bkey_ABCDEFGHIJKLMNOP; email joe@x.com"
	hits := Scan(text, all)

	seen := map[string]Hit{}
	for _, h := range hits {
		seen[h.Label] = h
	}
	if _, ok := seen["CUST_ID"]; !ok {
		t.Errorf("expected CUST_ID hit; got labels %v", labelSet(hits))
	}
	if _, ok := seen["INTERNAL_TOKEN"]; !ok {
		t.Errorf("expected INTERNAL_TOKEN hit; got labels %v", labelSet(hits))
	}
	if _, ok := seen["EMAIL"]; !ok {
		t.Errorf("built-in EMAIL still fires alongside custom rules; got %v", labelSet(hits))
	}
	// The per-rule action must land on the Hit so the proxy can consult it.
	if got := seen["CUST_ID"].Action; got != ActionRedact {
		t.Errorf("CUST_ID hit action = %q, want redact", got)
	}
	if got := seen["INTERNAL_TOKEN"].Action; got != ActionBlock {
		t.Errorf("INTERNAL_TOKEN hit action = %q, want block", got)
	}
	if got := seen["PROJECT_CODE"].Action; got != "" {
		t.Errorf("PROJECT_CODE hit action = %q, want \"\" (inherit)", got)
	}
}

func TestParsePolicy_ChecksumFiltersMatches(t *testing.T) {
	// AADHAAR-shaped rule with Verhoeff. A regex-only rule would match
	// every 12-digit sequence; adding `checksum: verhoeff` must filter
	// out the ones that don't pass the algorithm.
	policyYAML := `
version: v1
rules:
  - label: AADHAAR
    pattern: '\b\d{4}\s?\d{4}\s?\d{4}\b'
    checksum: verhoeff
`
	dets, _, err := ParsePolicy([]byte(policyYAML), "test.yaml")
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if len(dets) != 1 {
		t.Fatalf("expected 1 detector, got %d", len(dets))
	}
	d := dets[0]

	// Construct a Verhoeff-valid 12-digit vector deterministically.
	prefix := "12345678901"
	valid := fmt.Sprintf("%s%d", prefix, verhoeffCheckDigit(prefix))
	if hits := d.Detect("aadhaar " + valid + " for verification"); len(hits) != 1 {
		t.Errorf("expected 1 AADHAAR hit on valid Verhoeff %s, got %+v", valid, hits)
	}

	// Same shape, wrong check digit → the regex still matches, but the
	// checksum filter must drop it.
	invalid := fmt.Sprintf("%s%d", prefix, (verhoeffCheckDigit(prefix)+1)%10)
	if hits := d.Detect("aadhaar " + invalid + " for verification"); len(hits) != 0 {
		t.Errorf("expected 0 AADHAAR hits on non-Verhoeff %s, got %+v", invalid, hits)
	}
}

func TestParsePolicy_ChecksumLuhnFiltersCards(t *testing.T) {
	// Verify the Luhn checksum also works when referenced from a policy.
	// Users won't typically re-declare CREDIT_CARD (it's a built-in), but
	// custom bank-specific card rules could reasonably want it.
	policyYAML := `
version: v1
rules:
  - label: BANK_CARD
    pattern: '\b\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{4}\b'
    checksum: luhn
`
	dets, _, err := ParsePolicy([]byte(policyYAML), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Valid Luhn card
	if hits := dets[0].Detect("card 4242 4242 4242 4242"); len(hits) != 1 {
		t.Errorf("expected 1 hit on Luhn-valid card, got %+v", hits)
	}
	// Same shape, fails Luhn
	if hits := dets[0].Detect("card 1234 5678 9012 3456"); len(hits) != 0 {
		t.Errorf("expected 0 hits on Luhn-invalid card, got %+v", hits)
	}
}

func TestParsePolicy_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name:    "missing version",
			yaml:    "rules:\n  - label: X\n    pattern: 'a'\n",
			wantSub: "missing `version`",
		},
		{
			name:    "wrong version",
			yaml:    "version: v9\nrules:\n  - label: X\n    pattern: 'a'\n",
			wantSub: "unsupported version",
		},
		{
			name:    "no rules",
			yaml:    "version: v1\nrules: []\n",
			wantSub: "empty",
		},
		{
			name:    "empty label",
			yaml:    "version: v1\nrules:\n  - label: ''\n    pattern: 'a'\n",
			wantSub: "empty `label`",
		},
		{
			name:    "lowercase label",
			yaml:    "version: v1\nrules:\n  - label: cust_id\n    pattern: 'a'\n",
			wantSub: "must be uppercase",
		},
		{
			name:    "label collides with built-in",
			yaml:    "version: v1\nrules:\n  - label: EMAIL\n    pattern: 'a'\n",
			wantSub: "collides with a built-in",
		},
		{
			name:    "duplicate label",
			yaml:    "version: v1\nrules:\n  - label: X\n    pattern: 'a'\n  - label: X\n    pattern: 'b'\n",
			wantSub: "defined more than once",
		},
		{
			name:    "empty pattern",
			yaml:    "version: v1\nrules:\n  - label: X\n    pattern: ''\n",
			wantSub: "empty `pattern`",
		},
		{
			name:    "bad regex",
			yaml:    "version: v1\nrules:\n  - label: X\n    pattern: '['\n",
			wantSub: "invalid regex",
		},
		{
			name:    "unknown action",
			yaml:    "version: v1\nrules:\n  - label: X\n    pattern: 'a'\n    action: nuke\n",
			wantSub: "unknown action",
		},
		{
			name:    "unknown checksum",
			yaml:    "version: v1\nrules:\n  - label: X\n    pattern: 'a'\n    checksum: mycrc\n",
			wantSub: "unknown checksum",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ParsePolicy([]byte(c.yaml), "test.yaml")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q missing substring %q", err.Error(), c.wantSub)
			}
		})
	}
}

func labelSet(hits []Hit) []string {
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.Label] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// -----------------------------------------------------------------------------
// LoadPolicyFiles — multi-file composition
// -----------------------------------------------------------------------------

func TestLoadPolicyFiles_MergesRules(t *testing.T) {
	dir := t.TempDir()
	pack1 := filepath.Join(dir, "pack1.yaml")
	pack2 := filepath.Join(dir, "pack2.yaml")
	if err := os.WriteFile(pack1, []byte("version: v1\nrules:\n  - label: RULE_A\n    pattern: 'a+'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pack2, []byte("version: v1\nrules:\n  - label: RULE_B\n    pattern: 'b+'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dets, merged, err := LoadPolicyFiles([]string{pack1, pack2})
	if err != nil {
		t.Fatalf("LoadPolicyFiles: %v", err)
	}
	if len(dets) != 2 {
		t.Errorf("expected 2 detectors, got %d", len(dets))
	}
	if len(merged.Rules) != 2 {
		t.Errorf("expected 2 rules in merged policy, got %d", len(merged.Rules))
	}
	labels := SortedRuleLabels(merged)
	if labels[0] != "RULE_A" || labels[1] != "RULE_B" {
		t.Errorf("merged labels = %v, want [RULE_A RULE_B]", labels)
	}
}

func TestLoadPolicyFiles_RejectsCrossFileCollision(t *testing.T) {
	dir := t.TempDir()
	pack1 := filepath.Join(dir, "india.yaml")
	pack2 := filepath.Join(dir, "corp.yaml")
	// Both files define PAN. This should fail with a clear message that
	// names BOTH files so the user knows exactly which pack to fix.
	if err := os.WriteFile(pack1, []byte("version: v1\nrules:\n  - label: PAN\n    pattern: '[A-Z]{5}\\d{4}[A-Z]'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pack2, []byte("version: v1\nrules:\n  - label: PAN\n    pattern: 'CUSTOM'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadPolicyFiles([]string{pack1, pack2})
	if err == nil {
		t.Fatal("expected an error for cross-file label collision, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "PAN") {
		t.Errorf("collision error must mention the label: %q", msg)
	}
	if !strings.Contains(msg, "india.yaml") || !strings.Contains(msg, "corp.yaml") {
		t.Errorf("collision error must mention BOTH files: %q", msg)
	}
}

func TestLoadPolicyFiles_EmptyPathsReturnsNil(t *testing.T) {
	dets, p, err := LoadPolicyFiles(nil)
	if err != nil {
		t.Fatalf("nil paths must not error: %v", err)
	}
	if dets != nil || p != nil {
		t.Errorf("nil paths → both returns should be nil, got dets=%v p=%v", dets, p)
	}
}
