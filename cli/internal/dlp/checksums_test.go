// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"fmt"
	"testing"
)

func TestVerhoeff_KnownValid(t *testing.T) {
	// "2363" is the canonical 4-digit test vector from Verhoeff's original
	// paper: data "236" with check digit "3".
	if !verhoeffValid("2363") {
		t.Errorf("verhoeffValid(\"2363\") = false, want true (Verhoeff paper canonical example)")
	}
}

func TestVerhoeff_KnownInvalid(t *testing.T) {
	// Same prefix "236", wrong check digit → must fail.
	cases := []string{"2360", "2361", "2362", "2364", "2365", "2366", "2367", "2368", "2369"}
	for _, s := range cases {
		if verhoeffValid(s) {
			t.Errorf("verhoeffValid(%q) = true, want false (wrong check digit for prefix 236)", s)
		}
	}
	// Random 12-digit sequences generally do NOT pass Verhoeff — this is
	// what makes the algorithm useful for Aadhaar validation. We don't
	// assert a specific string (there IS a valid check digit for every
	// prefix), but we do the round-trip: pick a prefix, get the WRONG
	// check digit, verify it fails.
	prefix := "12345678901" // 11 digits — we'll append a wrong check
	correct := verhoeffCheckDigit(prefix)
	wrong := (correct + 1) % 10
	if verhoeffValid(fmt.Sprintf("%s%d", prefix, wrong)) {
		t.Errorf("expected Verhoeff to reject prefix %q with wrong check digit %d", prefix, wrong)
	}
}

func TestVerhoeff_RoundTrip_ArbitraryPrefixes(t *testing.T) {
	// For any prefix, appending verhoeffCheckDigit(prefix) must produce a
	// Verhoeff-valid string. This is the primary correctness invariant
	// and covers Aadhaar-length inputs without hard-coding vectors.
	prefixes := []string{
		"",             // check-only, degenerate but algorithm should handle
		"1",            // trivial
		"236",          // paper example prefix
		"12345678901",  // 11-digit prefix → 12-digit Aadhaar shape
		"999999999999", // repeating digits
		"01234567891",  // sequential
	}
	for _, p := range prefixes {
		c := verhoeffCheckDigit(p)
		if c < 0 || c > 9 {
			t.Errorf("verhoeffCheckDigit(%q) = %d, want 0-9", p, c)
			continue
		}
		full := fmt.Sprintf("%s%d", p, c)
		if !verhoeffValid(full) {
			t.Errorf("round-trip failed: prefix %q + check %d = %q, verhoeffValid returned false", p, c, full)
		}
	}
}

func TestVerhoeff_RejectsNonDigits(t *testing.T) {
	// Verhoeff is defined only over 0-9. Anything else should return false
	// rather than panic — the caller should have stripped separators.
	if verhoeffValid("abc") {
		t.Errorf("verhoeffValid(\"abc\") = true, want false")
	}
	if verhoeffValid("236 3") {
		t.Errorf("verhoeffValid(\"236 3\") = true, want false — spaces not stripped")
	}
}

func TestChecksumRegistry_HasAllNames(t *testing.T) {
	// The names below are exactly what policy YAML files reference via
	// `checksum:`. If someone renames one, this test forces them to also
	// update the docs and the preset packs.
	wantNames := []string{"luhn", "mod10", "verhoeff", "mod11"}
	for _, name := range wantNames {
		if _, ok := checksumByName[name]; !ok {
			t.Errorf("checksumByName missing %q — policy rules using this checksum will fail to load", name)
		}
	}
}

// -----------------------------------------------------------------------------
// Mod-11 (UK NHS variant)
// -----------------------------------------------------------------------------

func TestMod11_RoundTrip(t *testing.T) {
	// For any 9-digit prefix (skipping the ones where NHS-spec says the
	// number is invalid, i.e. check==10), appending mod11CheckDigit(prefix)
	// must produce a Mod-11-valid 10-digit string.
	prefixes := []string{
		"401023213", // known-good NHS test prefix
		"123456788", // arbitrary
		"999999999", // repeating
		"012345678", // sequential
	}
	tried := 0
	for _, p := range prefixes {
		c := mod11CheckDigit(p)
		if c < 0 {
			// This prefix produces a check-digit of 10 → NHS says invalid.
			// Skip; the round-trip invariant doesn't apply.
			continue
		}
		tried++
		full := fmt.Sprintf("%s%d", p, c)
		if !mod11Valid(full) {
			t.Errorf("round-trip failed: prefix %q + check %d = %q, mod11Valid returned false", p, c, full)
		}
	}
	if tried == 0 {
		t.Fatal("no test prefixes produced a valid check digit — sample set too small")
	}
}

func TestMod11_RejectsNonTenDigit(t *testing.T) {
	// UK NHS variant is defined for exactly 10 digits. Anything else
	// returns false rather than trying to guess weights.
	cases := []string{"", "1", "123", "12345678901", "abcdefghij"}
	for _, s := range cases {
		if mod11Valid(s) {
			t.Errorf("mod11Valid(%q) = true, want false", s)
		}
	}
}

func TestLuhnCheckDigit_RoundTrip(t *testing.T) {
	// For any digit prefix, appending LuhnCheckDigit(prefix) must produce
	// a Luhn-valid string. Same round-trip invariant as Verhoeff.
	prefixes := []string{
		"4",                     // trivial
		"4242424242424242"[:15], // 15-digit prefix → 16-digit valid card
		"411111111111111",       // 15-digit prefix
		"37828224631000",        // 14-digit prefix (Amex-shape)
	}
	for _, p := range prefixes {
		c := LuhnCheckDigit(p)
		if c < 0 || c > 9 {
			t.Errorf("LuhnCheckDigit(%q) = %d, want 0-9", p, c)
			continue
		}
		full := fmt.Sprintf("%s%d", p, c)
		if !luhnValid(full) {
			t.Errorf("Luhn round-trip failed: %s + %d = %q, luhnValid returned false", p, c, full)
		}
	}
}

func TestPublicCheckDigitWrappers(t *testing.T) {
	// The exported wrappers must return exactly what the unexported
	// versions do, so cli/cmd can compute checksums without reaching
	// into package internals.
	if VerhoeffCheckDigit("12345678901") != verhoeffCheckDigit("12345678901") {
		t.Error("VerhoeffCheckDigit disagrees with verhoeffCheckDigit")
	}
	if Mod11CheckDigit("401023213") != mod11CheckDigit("401023213") {
		t.Error("Mod11CheckDigit disagrees with mod11CheckDigit")
	}
}

func TestMod11_RejectsWrongCheckDigit(t *testing.T) {
	// Pick a prefix whose valid check digit isn't 10, then try every
	// wrong check digit and confirm all fail.
	prefix := "401023213"
	correct := mod11CheckDigit(prefix)
	if correct < 0 {
		t.Fatalf("test prefix %q happens to give check==10; pick a different prefix", prefix)
	}
	for wrong := 0; wrong <= 9; wrong++ {
		if wrong == correct {
			continue
		}
		s := fmt.Sprintf("%s%d", prefix, wrong)
		if mod11Valid(s) {
			t.Errorf("mod11Valid(%q) = true with wrong check %d (correct was %d)", s, wrong, correct)
		}
	}
}
