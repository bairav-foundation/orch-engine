// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

// This file collects the checksum validators used by policy rules. Each
// validator takes a digit-only string (caller has already stripped spaces
// and separators) and reports whether it passes the algorithm's check.
//
// The registry `checksumByName` lets a YAML rule reference a validator by
// name via the `checksum:` field:
//
//	rules:
//	  - label: AADHAAR
//	    pattern: '\b\d{4}\s?\d{4}\s?\d{4}\b'
//	    checksum: verhoeff
//
// When set, regexRuleDetector filters out regex matches whose digit-only
// form fails the named checksum. This is what stops an Aadhaar detector
// from false-positiving on every random 12-digit number.
//
// luhnValid lives in builtin.go (used by the CREDIT_CARD built-in). It's
// registered under both "luhn" (canonical) and "mod10" (common alias) so
// authors can use whichever name matches their mental model.

// ChecksumFn is the shape every checksum validator satisfies.
type ChecksumFn func(digits string) bool

// checksumByName is the registry of validators available to policy rules.
// Names must be lowercase; `Rule.Checksum` is compared case-insensitively.
var checksumByName = map[string]ChecksumFn{
	"luhn":     luhnValid,
	"mod10":    luhnValid, // alias — same algorithm, different colloquial name
	"verhoeff": verhoeffValid,
	"mod11":    mod11Valid, // UK NHS variant (see mod11Valid docstring)
}

// -----------------------------------------------------------------------------
// Verhoeff
// -----------------------------------------------------------------------------
//
// Verhoeff is a base-10 checksum designed to catch every single-digit
// transcription error and every adjacent transposition error. India's
// Aadhaar uses it; some Sri Lankan and Zimbabwean IDs do too. The
// algorithm relies on three lookup tables — d (Dihedral group D_5
// multiplication), p (permutation), inv (multiplicative inverse). This
// is the standard reference implementation.

// verhoeffD is the multiplication table for the dihedral group D_5.
var verhoeffD = [10][10]int{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	{1, 2, 3, 4, 0, 6, 7, 8, 9, 5},
	{2, 3, 4, 0, 1, 7, 8, 9, 5, 6},
	{3, 4, 0, 1, 2, 8, 9, 5, 6, 7},
	{4, 0, 1, 2, 3, 9, 5, 6, 7, 8},
	{5, 9, 8, 7, 6, 0, 4, 3, 2, 1},
	{6, 5, 9, 8, 7, 1, 0, 4, 3, 2},
	{7, 6, 5, 9, 8, 2, 1, 0, 4, 3},
	{8, 7, 6, 5, 9, 3, 2, 1, 0, 4},
	{9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
}

// verhoeffP is the permutation table (rows are successive powers of the
// permutation, applied by position).
var verhoeffP = [8][10]int{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	{1, 5, 7, 6, 2, 8, 3, 0, 9, 4},
	{5, 8, 0, 3, 7, 9, 6, 1, 4, 2},
	{8, 9, 1, 6, 0, 4, 3, 5, 2, 7},
	{9, 4, 5, 3, 1, 2, 6, 8, 7, 0},
	{4, 2, 8, 6, 5, 7, 3, 9, 0, 1},
	{2, 7, 9, 3, 8, 0, 6, 4, 1, 5},
	{7, 0, 4, 6, 9, 1, 3, 2, 5, 8},
}

// verhoeffInv is the multiplicative inverse table for D_5. Used only by
// verhoeffCheckDigit (below) — verification doesn't need it because we
// check "did the accumulator reach 0" instead of "recover the digit."
var verhoeffInv = [10]int{0, 4, 3, 2, 1, 5, 6, 7, 8, 9}

// verhoeffValid reports whether digits (an ASCII '0'-'9' string, no
// separators) passes the Verhoeff check. The check digit is the last
// digit; verification walks the digits right-to-left, composing them
// through D and P, and returns true iff the running value is zero.
func verhoeffValid(digits string) bool {
	if len(digits) == 0 {
		return false
	}
	c := 0
	// Walk right-to-left; index 0 in the walk is the last digit.
	for i := 0; i < len(digits); i++ {
		d := digits[len(digits)-1-i]
		if d < '0' || d > '9' {
			return false
		}
		c = verhoeffD[c][verhoeffP[i%8][int(d-'0')]]
	}
	return c == 0
}

// verhoeffCheckDigit computes the Verhoeff check digit for the given data
// string. Appending the returned digit to data produces a valid Verhoeff-
// checked string (verhoeffValid returns true for the concatenation).
//
// Now also used by the public `bairav dlp checksum` helper (see cmd/dlp.go)
// so rule authors can generate valid test vectors without hand-computing.
func verhoeffCheckDigit(data string) int {
	c := 0
	// Walk right-to-left, but the check-digit position is i=0 (empty for
	// now), so real data starts at i=1.
	for i := 0; i < len(data); i++ {
		d := data[len(data)-1-i]
		if d < '0' || d > '9' {
			return -1
		}
		c = verhoeffD[c][verhoeffP[(i+1)%8][int(d-'0')]]
	}
	return verhoeffInv[c]
}

// -----------------------------------------------------------------------------
// Mod-11 (UK NHS variant)
// -----------------------------------------------------------------------------
//
// Mod-11 is a family of algorithms; we ship the UK NHS variant because
// it's the most common in identification numbers and the closest thing
// to a "generic" mod-11. Weights for a 10-digit number are 10..2 for
// positions 1..9, with the 10th digit as the check.
//
// Algorithm:
//   sum = Σ digit_i × (11 - i)  for i = 1..9
//   check = 11 - (sum mod 11)
//   If check == 11, treat as 0.
//   If check == 10, the number is INVALID (NHS never issues these).
//   Otherwise the check digit must equal the 10th digit for validity.
//
// Reference: UK Data Dictionary Service NHS number specification.
// Also matches ISO 7064 Mod 11-10 for 10-digit strings.

// mod11Valid reports whether digits (a 10-digit ASCII string) passes
// the UK NHS Mod-11 check. Digits must be exactly 10 chars; other
// lengths return false rather than trying to guess weights.
func mod11Valid(digits string) bool {
	if len(digits) != 10 {
		return false
	}
	sum := 0
	for i := 0; i < 9; i++ {
		c := digits[i]
		if c < '0' || c > '9' {
			return false
		}
		sum += int(c-'0') * (10 - i)
	}
	remainder := sum % 11
	check := 11 - remainder
	switch check {
	case 11:
		check = 0
	case 10:
		// NHS spec: check digit of 10 means the number is not valid.
		// Never issued.
		return false
	}
	last := digits[9]
	if last < '0' || last > '9' {
		return false
	}
	return check == int(last-'0')
}

// -----------------------------------------------------------------------------
// Public check-digit computers (used by `bairav dlp checksum`)
// -----------------------------------------------------------------------------
//
// These are thin wrappers so the cli/cmd package can compute check
// digits without reaching into unexported package internals. Kept
// separate from the unexported originals so tests continue to use the
// short names and the public surface stays intentional.

// VerhoeffCheckDigit exposes verhoeffCheckDigit for the CLI helper.
// Returns -1 on any non-digit input.
func VerhoeffCheckDigit(prefix string) int { return verhoeffCheckDigit(prefix) }

// Mod11CheckDigit exposes mod11CheckDigit for the CLI helper. Returns
// -1 if the prefix isn't exactly 9 digits or if the algorithm produces
// a check of 10 (NHS-invalid — pick a different prefix).
func Mod11CheckDigit(prefix string) int { return mod11CheckDigit(prefix) }

// LuhnCheckDigit computes the Luhn check digit for a digits-only prefix
// of any length ≥ 1. Returns -1 on non-digit input. The full valid
// string is prefix + LuhnCheckDigit(prefix).
//
// Algorithm: same as luhnValid but with the check position placeholder
// at position 0 (rightmost). Compute sum over the prefix (treated as if
// the check is 0), then the check digit is (10 - sum%10) mod 10.
func LuhnCheckDigit(prefix string) int {
	if len(prefix) == 0 {
		return -1
	}
	sum := 0
	// Walk right-to-left. Because the check digit (position 0) will be
	// appended, its position parity flips the alternation: the first
	// real digit (currently rightmost) will end up in position 1 in the
	// final string — i.e. it should be doubled.
	alt := true
	for i := len(prefix) - 1; i >= 0; i-- {
		c := prefix[i]
		if c < '0' || c > '9' {
			return -1
		}
		n := int(c - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return (10 - sum%10) % 10
}

// mod11CheckDigit computes the NHS Mod-11 check digit for a 9-digit
// prefix. Returns -1 if the prefix isn't exactly 9 digits or if the
// computed check comes out to 10 (in which case NHS spec says the whole
// number is unusable — pick a different prefix). Used by the
// `bairav dlp checksum` helper.
func mod11CheckDigit(prefix string) int {
	if len(prefix) != 9 {
		return -1
	}
	sum := 0
	for i := 0; i < 9; i++ {
		c := prefix[i]
		if c < '0' || c > '9' {
			return -1
		}
		sum += int(c-'0') * (10 - i)
	}
	remainder := sum % 11
	check := 11 - remainder
	switch check {
	case 11:
		return 0
	case 10:
		return -1
	}
	return check
}
