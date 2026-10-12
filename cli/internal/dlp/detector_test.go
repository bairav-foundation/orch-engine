// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"reflect"
	"strings"
	"testing"
)

func TestEmailDetector(t *testing.T) {
	pos := []string{
		"reach me at alice@example.com please",
		"alice.bob+filter@sub.example.co.uk",
		"a@b.io",
	}
	neg := []string{
		"no email here",
		"not.an.email",
		"@nope",
		"bare-at-without-domain@",
	}
	d := emailDetector{}
	for _, s := range pos {
		if hits := d.Detect(s); len(hits) == 0 {
			t.Errorf("email detector: expected hit in %q", s)
		}
	}
	for _, s := range neg {
		if hits := d.Detect(s); len(hits) != 0 {
			t.Errorf("email detector: unexpected hit in %q: %+v", s, hits)
		}
	}
}

func TestSSNDetector(t *testing.T) {
	d := ssnDetector{}
	if hits := d.Detect("ssn 123-45-6789 in body"); len(hits) != 1 {
		t.Errorf("expected one SSN hit, got %+v", hits)
	}
	// Pure 9-digit run must NOT trigger — too many false positives on order IDs.
	if hits := d.Detect("order #123456789 was shipped"); len(hits) != 0 {
		t.Errorf("expected no SSN hit on plain 9-digit, got %+v", hits)
	}
}

func TestCreditCardDetector_LuhnValidatesCard(t *testing.T) {
	d := creditCardDetector{}
	// Valid Luhn (real test cards from card-test suites).
	cases := []string{
		"my card is 4242 4242 4242 4242 thanks",
		"4111-1111-1111-1111",
		"5555555555554444",
		"378282246310005", // amex 15-digit
	}
	for _, s := range cases {
		if hits := d.Detect(s); len(hits) == 0 {
			t.Errorf("expected CREDIT_CARD hit in %q", s)
		}
	}
	// Sanity — the label emitted is the new one, not the historical "PAN".
	hits := d.Detect("card 4242 4242 4242 4242")
	if len(hits) == 0 || hits[0].Label != "CREDIT_CARD" {
		t.Errorf("expected label CREDIT_CARD, got %+v", hits)
	}
	// Same number of digits but FAILS Luhn → must not trigger.
	if hits := d.Detect("not a card: 1234 5678 9012 3456"); len(hits) != 0 {
		t.Errorf("expected no CREDIT_CARD hit on non-Luhn 16-digit, got %+v", hits)
	}
	// Too short / too long → must not trigger.
	if hits := d.Detect("phone-ish: 555 123 4567"); len(hits) != 0 {
		t.Errorf("expected no CREDIT_CARD hit on short number, got %+v", hits)
	}
}

func TestUSPhoneDetector(t *testing.T) {
	d := usPhoneDetector{}
	pos := []string{
		"(415) 555-1212",
		"415-555-1212",
		"415.555.1212",
		"415 555 1212",
		"+1 415-555-1212",
	}
	for _, s := range pos {
		if hits := d.Detect(s); len(hits) == 0 {
			t.Errorf("expected phone hit in %q", s)
		}
	}
	// Single bare digit run is short enough that we DO match it; the v1
	// trade-off favours recall. If this becomes a problem we'll tighten
	// to require punctuation.
}

func TestAWSAccessKeyDetector(t *testing.T) {
	d := awsAccessKeyDetector{}
	pos := []string{
		"export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
		"creds: ASIAIOSFODNN7EXAMPLE",
	}
	neg := []string{
		"no key here",
		"AKIA123",                    // too short
		"hello AKIAioSFODNN7example", // lowercase letters → not a real AWS key
	}
	for _, s := range pos {
		if hits := d.Detect(s); len(hits) == 0 {
			t.Errorf("expected AWS hit in %q", s)
		}
	}
	for _, s := range neg {
		if hits := d.Detect(s); len(hits) != 0 {
			t.Errorf("unexpected AWS hit in %q: %+v", s, hits)
		}
	}
}

func TestScan_DedupesAndOrders(t *testing.T) {
	text := "ping alice@example.com call (415) 555-1212"
	hits := Scan(text, DefaultDetectors())
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d: %+v", len(hits), hits)
	}
	// Sorted by Start.
	if hits[0].Start >= hits[1].Start {
		t.Errorf("hits not sorted by Start: %+v", hits)
	}
	if hits[0].Label != "EMAIL" || hits[1].Label != "PHONE_US" {
		t.Errorf("unexpected labels: %v", []string{hits[0].Label, hits[1].Label})
	}
}

func TestRedact_PlaceholdersReplaceSpans(t *testing.T) {
	text := "email alice@example.com SSN 123-45-6789"
	hits := Scan(text, DefaultDetectors())
	out := Redact(text, hits)

	// No raw PII remains.
	if strings.Contains(out, "alice@example.com") {
		t.Errorf("email leaked into redacted output: %q", out)
	}
	if strings.Contains(out, "123-45-6789") {
		t.Errorf("ssn leaked into redacted output: %q", out)
	}
	// Placeholders appear in the right order.
	emailIdx := strings.Index(out, "[REDACTED:EMAIL]")
	ssnIdx := strings.Index(out, "[REDACTED:SSN]")
	if emailIdx < 0 || ssnIdx < 0 {
		t.Fatalf("placeholders missing: %q", out)
	}
	if emailIdx >= ssnIdx {
		t.Errorf("placeholders out of order in: %q", out)
	}
	// Surrounding text preserved verbatim.
	if !strings.HasPrefix(out, "email ") {
		t.Errorf("prefix lost: %q", out)
	}
}

func TestRedact_HandlesNoHitsGracefully(t *testing.T) {
	const s = "nothing sensitive here"
	if got := Redact(s, nil); got != s {
		t.Errorf("Redact with no hits should return input verbatim, got %q", got)
	}
}

func TestLabels(t *testing.T) {
	hits := []Hit{
		{Label: "EMAIL"},
		{Label: "SSN"},
		{Label: "EMAIL"},
		{Label: "CREDIT_CARD"},
	}
	got := Labels(hits)
	want := []string{"CREDIT_CARD", "EMAIL", "SSN"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Labels = %v, want %v", got, want)
	}
}

func TestLuhnValid_KnownCards(t *testing.T) {
	good := []string{
		"4242424242424242", // Visa test card
		"4111111111111111",
		"5555555555554444", // Mastercard test
		"378282246310005",  // Amex test
	}
	for _, s := range good {
		if !luhnValid(s) {
			t.Errorf("luhnValid(%q) = false, want true", s)
		}
	}
	bad := []string{
		"4242424242424243",
		"1234567890123456",
		"",
	}
	for _, s := range bad {
		if luhnValid(s) {
			t.Errorf("luhnValid(%q) = true, want false", s)
		}
	}
}
