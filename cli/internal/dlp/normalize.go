// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

// Text normalisation before detection.
//
// Detectors are ASCII regexes. Without this step, trivial rewrites slip
// past every one of them:
//
//	４１１１ １１１１ １１１１ １１１１     full-width digits
//	4111<U+200B>1111<U+200B>...  zero-width spaces between groups
//	२३४५ ६७८९ ०१२४                         Devanagari digits (also Tamil, Bengali, Arabic-Indic)
//
// fold maps such text to an ASCII-equivalent copy plus an offset table, so
// a hit found in the folded text can be mapped back to the exact span of
// the original (which is what gets redacted). Plain ASCII input takes a
// fast path and is scanned as-is.

import (
	"unicode/utf8"
)

// zeroWidth are format characters that render as nothing and are commonly
// used to break up a number so regexes miss it.
var zeroWidth = map[rune]bool{
	'\u200b': true, // zero width space
	'\u200c': true, // zero width non-joiner
	'\u200d': true, // zero width joiner
	'\u2060': true, // word joiner
	'\ufeff': true, // zero width no-break space / BOM
	'\u00ad': true, // soft hyphen
}

// digitZeros are the code points of '0' in decimal-digit blocks whose
// digits run contiguously 0..9. Chosen for the regions the packs target.
var digitZeros = []rune{
	0x0660, // Arabic-Indic
	0x06F0, // Extended Arabic-Indic (Persian, Urdu)
	0x0966, // Devanagari (Hindi, Marathi, ...)
	0x09E6, // Bengali
	0x0A66, // Gurmukhi
	0x0AE6, // Gujarati
	0x0B66, // Oriya
	0x0BE6, // Tamil
	0x0C66, // Telugu
	0x0CE6, // Kannada
	0x0D66, // Malayalam
	0xFF10, // Full-width
}

// foldRune returns the ASCII replacement for r, or (0, true) when r should
// be dropped, or (r, false) when r is kept unchanged.
func foldRune(r rune) (rune, bool) {
	if zeroWidth[r] {
		return 0, true
	}
	for _, z := range digitZeros {
		if r >= z && r <= z+9 {
			return '0' + (r - z), false
		}
	}
	// Full-width ASCII block (！..～): letters, @, -, . etc.
	if r >= 0xFF01 && r <= 0xFF5E {
		return r - 0xFEE0, false
	}
	if r == 0x3000 { // ideographic space
		return ' ', false
	}
	return r, false
}

// needsFold reports whether any rune in s would be changed by foldRune.
func needsFold(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			for _, r := range s[i:] {
				if f, drop := foldRune(r); drop || f != r {
					return true
				}
			}
			return false
		}
	}
	return false
}

// fold returns the folded text and, for every byte of it, the byte offset
// in the original where that byte's source rune starts (starts) and ends
// (ends). starts/ends are nil when no folding was needed.
func fold(s string) (folded string, starts, ends []int) {
	if !needsFold(s) {
		return s, nil, nil
	}
	buf := make([]byte, 0, len(s))
	starts = make([]int, 0, len(s))
	ends = make([]int, 0, len(s))
	for i, r := range s {
		size := utf8.RuneLen(r)
		if size < 0 {
			size = 1
		}
		f, drop := foldRune(r)
		if drop {
			continue
		}
		before := len(buf)
		buf = utf8.AppendRune(buf, f)
		for j := before; j < len(buf); j++ {
			starts = append(starts, i)
			ends = append(ends, i+size)
		}
	}
	return string(buf), starts, ends
}

// mapBack translates a hit found in folded text to original offsets.
func mapBack(h Hit, original string, starts, ends []int) Hit {
	if starts == nil || h.End <= h.Start {
		return h
	}
	h.Start = starts[h.Start]
	h.End = ends[h.End-1]
	h.Match = original[h.Start:h.End]
	return h
}
