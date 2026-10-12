// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"regexp"
	"strconv"
)

// Tier classifies a served model by approximate parameter count, so the CLI
// can set user expectations up front (e.g. "this is a tiny model — don't
// expect deep reasoning") and recommend upgrades when appropriate.
//
// Tiering is heuristic on purpose: we parse the filename / repo slug for a
// `<N>B` token, then bucket. We don't introspect GGUF metadata (would mean
// reading the file header and parsing its KV store — heavy for a startup hint).
type Tier struct {
	Name     string  // "tiny", "small", "standard", "large", "xlarge", or "unknown"
	Params   float64 // billions of parameters; 0 if unknown
	UseCases string  // one-line human-readable description
}

// paramTokenRe matches "<digits>[.digits]B" anywhere in a string. Case
// insensitive. Examples it catches: "0.5B", "7B", "8b", "70B".
var paramTokenRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)b\b`)

// ParseParamSize extracts the first "<N>B" token from s and returns N as a
// float (in billions). Returns 0 if no token is found.
func ParseParamSize(s string) float64 {
	m := paramTokenRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	return v
}

// Classify returns a Tier for a model, looking at the filename first (more
// likely to be precise — `qwen2.5-0.5b-instruct-q4_k_m.gguf`) and falling
// back to the repo slug (`Qwen/Qwen2.5-7B-Instruct-GGUF`).
func Classify(filename, repo string) Tier {
	p := ParseParamSize(filename)
	if p == 0 {
		p = ParseParamSize(repo)
	}
	return tierFor(p)
}

func tierFor(p float64) Tier {
	switch {
	case p == 0:
		return Tier{Name: "unknown", Params: 0,
			UseCases: "size unknown — quality may vary"}
	case p < 1:
		return Tier{Name: "tiny", Params: p,
			UseCases: "fast Q&A; not for code, finance, or long reasoning"}
	case p < 3:
		return Tier{Name: "small", Params: p,
			UseCases: "general Q&A, simple code; struggles with deep reasoning"}
	case p < 10:
		return Tier{Name: "standard", Params: p,
			UseCases: "code, summarisation, multi-turn chat — solid daily driver"}
	case p < 30:
		return Tier{Name: "large", Params: p,
			UseCases: "complex reasoning, long context; needs ~16-24GB RAM/VRAM"}
	default:
		return Tier{Name: "xlarge", Params: p,
			UseCases: "frontier reasoning, math, code; serious GPU recommended"}
	}
}

// SuggestedUpgrade returns a known-good larger model when the current tier
// is below "standard". Empty string otherwise — we don't badger users on
// already-capable models.
func (t Tier) SuggestedUpgrade() string {
	if t.Name == "tiny" {
		return "bartowski/Qwen2.5-7B-Instruct-GGUF"
	}
	return ""
}

// FormatParams renders Params as a short human string: "0.5", "7", "27", "70".
func (t Tier) FormatParams() string {
	if t.Params == 0 {
		return "?"
	}
	if t.Params == float64(int(t.Params)) {
		return strconv.Itoa(int(t.Params))
	}
	return strconv.FormatFloat(t.Params, 'f', 1, 64)
}
