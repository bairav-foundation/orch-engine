// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package packs

import (
	"testing"
)

func TestPackNames_IncludesGlobalSafeAndAtLeastOneRegional(t *testing.T) {
	names := PackNames()
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	if !seen["global-safe"] {
		t.Errorf("PackNames missing global-safe — this is the default pack, must always ship")
	}
	// At least one regional pack must ship so the --pack UX has
	// something to demonstrate on Day 12.
	regional := 0
	for _, n := range names {
		if n != "global-safe" {
			regional++
		}
	}
	if regional == 0 {
		t.Errorf("PackNames has no regional packs — expected at least india + uk")
	}
}

func TestLoad_GlobalSafeParses(t *testing.T) {
	p, err := Load("global-safe")
	if err != nil {
		t.Fatalf("Load(global-safe): %v", err)
	}
	if p.Policy == nil || len(p.Policy.Rules) == 0 {
		t.Fatal("global-safe policy must have rules")
	}
	// AADHAAR is the canonical checksum-anchored rule; guard against
	// someone accidentally removing it.
	hasAadhaar := false
	for _, r := range p.Policy.Rules {
		if r.Label == "AADHAAR" {
			hasAadhaar = true
			if r.Checksum == "" {
				t.Errorf("AADHAAR in global-safe must have a checksum — it's the whole reason it's safe by default")
			}
		}
	}
	if !hasAadhaar {
		t.Error("global-safe expected to contain AADHAAR")
	}
}

func TestLoad_UnknownPackErrors(t *testing.T) {
	_, err := Load("nonexistent-pack")
	if err == nil {
		t.Fatal("Load(unknown) should error")
	}
}

func TestLoadMany_MergesPacks(t *testing.T) {
	// global-safe + india: no label collisions, both should load.
	dets, p, err := LoadMany([]string{"global-safe", "india"})
	if err != nil {
		t.Fatalf("LoadMany: %v", err)
	}
	if len(dets) == 0 {
		t.Fatal("expected combined detectors, got none")
	}
	if p == nil || len(p.Rules) < 2 {
		t.Fatalf("expected merged policy with multiple rules, got %+v", p)
	}
}

func TestLoadMany_EmptyReturnsNil(t *testing.T) {
	dets, p, err := LoadMany(nil)
	if err != nil {
		t.Fatalf("nil paths must not error: %v", err)
	}
	if dets != nil || p != nil {
		t.Errorf("empty pack list → nil returns; got dets=%v p=%v", dets, p)
	}
}

func TestGlobalSafeContainsOnlyChecksumRules(t *testing.T) {
	// The whole "safe by default" story rests on global-safe containing
	// only checksum-anchored rules. If someone accidentally adds a
	// shape-only rule here, false-positive rates jump for every user.
	// This test is the regression guard for that decision.
	p, err := Load("global-safe")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Policy.Rules {
		if r.Checksum == "" {
			t.Errorf("global-safe rule %q has no checksum — belongs in a regional pack, not global-safe", r.Label)
		}
	}
}
