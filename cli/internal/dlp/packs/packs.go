// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package packs holds the built-in DLP policy bundles that ship compiled
// into the CLI binary. Each pack is a YAML file in this directory; //go:embed
// bakes them in at build time so the binary is self-contained (no on-disk
// files needed, no "did the user copy the yaml correctly" failure mode).
//
// Pack semantics:
//
//	global-safe — on by default. Every rule has a checksum, so the
//	              false-positive rate is near zero regardless of the
//	              caller's geography. Safe for multinationals.
//	india, uk, eu, ... — regional shape-only rules. Opt-in per geography
//	                     because bare-regex rules have real FP cost when
//	                     enabled outside the region they target.
//
// The runtime consumer is cli/cmd/dlp.go, which calls PackNames() to
// enumerate and Load(name) to get the compiled detectors for a specific
// pack. Users still author their own YAML files via --policy; packs are
// just the curated starting points.
package packs

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/bairav-foundation/orch-engine/cli/internal/dlp"
)

//go:embed *.yaml
var packFS embed.FS

// Pack is one embedded bundle after parsing. Detectors is what gets
// merged with the user's --policy files and the built-in DefaultDetectors
// inside the proxy; Policy is retained so `bairav dlp packs describe`
// can print rule details without re-parsing.
type Pack struct {
	Name      string
	Detectors []dlp.Detector
	Policy    *dlp.Policy
}

// PackNames returns every embedded pack name, sorted, without loading
// (compiling regexes) any of them. Used by the CLI's `dlp packs list`
// and by discovery UI.
func PackNames() []string {
	entries, err := fs.ReadDir(packFS, ".")
	if err != nil {
		// Shouldn't happen — the FS is embedded — but be defensive so
		// startup never panics from a bad go:embed at runtime.
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ".yaml"))
	}
	sort.Strings(out)
	return out
}

// Load parses an embedded pack by name and returns its detectors +
// parsed Policy. Returns an error if the pack doesn't exist or fails
// validation (which should never happen for the shipped packs — they're
// tested at build time — but keeps the API honest for future dynamic
// packs).
func Load(name string) (*Pack, error) {
	body, err := packFS.ReadFile(name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("dlp packs: unknown pack %q (available: %s)",
			name, strings.Join(PackNames(), ", "))
	}
	dets, p, err := dlp.ParsePolicy(body, "packs/"+name+".yaml")
	if err != nil {
		return nil, fmt.Errorf("dlp packs: %s: %w", name, err)
	}
	return &Pack{Name: name, Detectors: dets, Policy: p}, nil
}

// LoadMany parses multiple packs and merges their detectors. Cross-pack
// label collisions are rejected with a clear error naming both packs, so
// a user assembling packs (--pack india,corp-custom) hears about
// ambiguities immediately. Same shape/semantics as dlp.LoadPolicyFiles.
func LoadMany(names []string) ([]dlp.Detector, *dlp.Policy, error) {
	if len(names) == 0 {
		return nil, nil, nil
	}
	labelSource := map[string]string{}
	merged := &dlp.Policy{Version: dlp.PolicyVersion}
	var all []dlp.Detector
	for _, name := range names {
		p, err := Load(name)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range p.Policy.Rules {
			if other, dup := labelSource[r.Label]; dup {
				return nil, nil, fmt.Errorf(
					"dlp packs: label %q is defined in both %q and %q packs — remove one from your --pack list",
					r.Label, other, name)
			}
			labelSource[r.Label] = name
			merged.Rules = append(merged.Rules, r)
		}
		all = append(all, p.Detectors...)
	}
	return all, merged, nil
}
