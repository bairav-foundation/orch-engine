// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"fmt"
	"sort"
	"strings"
)

// Hit is one span of detected sensitive content inside a piece of text.
// Start and End are byte offsets into the original text (End exclusive).
type Hit struct {
	// Label is the canonical detector name, e.g. "EMAIL", "SSN", "PAN".
	// Used in the redaction placeholder and the audit log.
	Label string `json:"label"`

	// Start is the byte offset of the first byte of the matched span.
	Start int `json:"start"`

	// End is the byte offset just past the matched span (exclusive).
	End int `json:"end"`

	// Match is the actual matched substring. We log it in audit (so SOC
	// teams can review false positives), but never echo it back to the
	// user or include it in the redacted output.
	Match string `json:"match"`

	// Action is an OPTIONAL per-hit action override. Empty means "inherit
	// the proxy's global --action". User-defined YAML rules can set this
	// so a policy can say "redact emails, block credit cards". Built-in
	// detectors leave this empty; only rules loaded from Policy set it.
	Action Action `json:"action,omitempty"`
}

// Detector is any rule capable of finding spans of sensitive content.
// Implementations must be safe for concurrent use — the proxy calls
// them on every request.
type Detector interface {
	// Name returns the canonical label used in Hit.Label and audit logs.
	Name() string

	// Detect returns every match in text. May return overlapping ranges;
	// Scan dedupes and orders them.
	Detect(text string) []Hit
}

// Scan runs every detector against text and returns the union of their
// hits, sorted by Start and with overlaps removed. When two hits overlap,
// the earlier-starting one wins; ties go to the longer match (so a Luhn-
// validated PAN beats a bare digit run at the same start).
//
// Text is folded first (normalize.go: full-width and Indic digits to
// ASCII, zero-width characters dropped); hit offsets always refer to the
// original text.
func Scan(text string, detectors []Detector) []Hit {
	folded, starts, ends := fold(text)
	var all []Hit
	for _, d := range detectors {
		for _, h := range d.Detect(folded) {
			all = append(all, mapBack(h, text, starts, ends))
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Start != all[j].Start {
			return all[i].Start < all[j].Start
		}
		// Same start: prefer the longer match.
		return (all[i].End - all[i].Start) > (all[j].End - all[j].Start)
	})
	// Drop hits whose span overlaps an earlier accepted hit.
	out := all[:0]
	prevEnd := -1
	for _, h := range all {
		if h.Start < prevEnd {
			continue
		}
		out = append(out, h)
		prevEnd = h.End
	}
	return out
}

// Redact rewrites text by replacing each hit's span with the placeholder
// "[REDACTED:LABEL]". Hits must be non-overlapping (Scan guarantees that).
// Processes right-to-left so we don't have to track offset drift.
func Redact(text string, hits []Hit) string {
	if len(hits) == 0 {
		return text
	}
	// Process right-to-left so earlier byte offsets remain valid as we go.
	sort.Slice(hits, func(i, j int) bool { return hits[i].Start > hits[j].Start })
	var b strings.Builder
	b.Grow(len(text))
	cursor := len(text)
	pieces := make([]string, 0, len(hits)*2+1)
	for _, h := range hits {
		// Slice from h.End up to cursor — that's the unmodified tail.
		pieces = append(pieces, text[h.End:cursor])
		pieces = append(pieces, fmt.Sprintf("[REDACTED:%s]", h.Label))
		cursor = h.Start
	}
	// Whatever is left at the head.
	pieces = append(pieces, text[:cursor])
	// pieces is in reverse order — reassemble.
	for i := len(pieces) - 1; i >= 0; i-- {
		b.WriteString(pieces[i])
	}
	return b.String()
}

// Labels returns the unique set of labels in hits, sorted, for audit.
func Labels(hits []Hit) []string {
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.Label] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
