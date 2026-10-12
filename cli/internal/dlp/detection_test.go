// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"strings"
	"testing"
)

func labelsOf(text string) string {
	var l []string
	for _, h := range Scan(text, DefaultDetectors()) {
		l = append(l, h.Label+"="+h.Match)
	}
	return strings.Join(l, ",")
}

func TestDetectionEvasions(t *testing.T) {
	for in, want := range map[string]string{
		"card 4111 1111 1111 1111":                "CREDIT_CARD=4111 1111 1111 1111",
		"card 4111111111111111 123":               "CREDIT_CARD=4111111111111111",
		"exp 12/27 card 4111-1111-1111-1111 cvv":  "CREDIT_CARD=4111-1111-1111-1111",
		"card 4111.1111.1111.1111":                "CREDIT_CARD=4111.1111.1111.1111",
		"card ４１１１１１１１１１１１１１１１":                   "CREDIT_CARD=４１１１１１１１１１１１１１１１",
		"card 4111\u200b1111\u200b1111\u200b1111": "CREDIT_CARD=4111\u200b1111\u200b1111\u200b1111",
		"ssn 123 45 6789":                         "SSN=123 45 6789",
		"SSN: 123456789":                          "SSN=123456789",
		"order 123456789":                         "", // bare 9 digits without an SSN keyword
		"ssn 000-12-3456":                         "", // never issued
		"ref 123-45 6789 x":                       "", // mixed separators: not an SSN layout
	} {
		if got := labelsOf(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestSecrets(t *testing.T) {
	for in, label := range map[string]string{
		"sk-proj-" + strings.Repeat("a1B2", 12):                                                "OPENAI_KEY",
		"sk-ant-api03-" + strings.Repeat("x", 90):                                              "ANTHROPIC_KEY",
		"ghp_" + strings.Repeat("A", 36):                                                       "GITHUB_TOKEN",
		"github_pat_" + strings.Repeat("a", 82):                                                "GITHUB_TOKEN",
		"xoxb-123456789012-abcdefghij":                                                         "SLACK_TOKEN",
		"sk_live_" + strings.Repeat("Z", 24):                                                   "STRIPE_KEY",
		"AIza" + strings.Repeat("Q", 35):                                                       "GOOGLE_API_KEY",
		"hf_" + strings.Repeat("b", 34):                                                        "HF_TOKEN",
		"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY":                     "AWS_SECRET_KEY",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N":              "JWT",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaA==\n-----END OPENSSH PRIVATE KEY-----": "PRIVATE_KEY",
	} {
		hs := Scan("leaked: "+in+" ok", DefaultDetectors())
		if len(hs) != 1 || hs[0].Label != label {
			t.Errorf("%.30q…: got %+v, want one %s", in, hs, label)
		}
	}
	// Look-alikes that must not fire.
	for _, in := range []string{"sk-learn is a library", "the hf_ prefix", "eyJ alone", "AIza short"} {
		if hs := Scan(in, DefaultDetectors()); len(hs) != 0 {
			t.Errorf("%q: false positive %+v", in, hs)
		}
	}
}

func TestFoldMapsBackToOriginal(t *testing.T) {
	in := "Aadhaar २३४५ ६७८९ ०१२४ and mail a@b.co"
	hs := Scan(in, DefaultDetectors())
	if len(hs) != 1 || hs[0].Match != "a@b.co" {
		t.Fatalf("built-ins only: %+v", hs)
	}
	if got := Redact(in, hs); got != "Aadhaar २३४५ ६७८९ ०१२४ and mail [REDACTED:EMAIL]" {
		t.Errorf("redaction offsets wrong after folding: %q", got)
	}
}
