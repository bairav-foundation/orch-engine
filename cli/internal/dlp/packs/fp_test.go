// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package packs

// False-positive and recall benchmark for global-safe. A one-digit
// checksum passes ~10% of random numbers, so shape+checksum alone flagged
// ~10% of order numbers as AADHAAR and ~9% of phone numbers as UK_NHS
// (external teardown, Oct 2026). Context rules must keep that under 1%
// without losing real IDs written the way people write them.

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/bairav-foundation/orch-engine/cli/internal/dlp"
)

func rate(t *testing.T, dets []dlp.Detector, label string, gen func() string) float64 {
	t.Helper()
	n, hit := 20000, 0
	for i := 0; i < n; i++ {
		for _, h := range dlp.Scan(gen(), dets) {
			if h.Label == label {
				hit++
				break
			}
		}
	}
	return 100 * float64(hit) / float64(n)
}

func TestGlobalSafeFalsePositives(t *testing.T) {
	dets, _, err := LoadMany([]string{"global-safe"})
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(7))
	for _, c := range []struct {
		label string
		gen   func() string
	}{
		{"AADHAAR", func() string { return fmt.Sprintf("order %012d shipped", r.Int63n(1e12)) }},
		{"AADHAAR", func() string { return fmt.Sprintf("txn %d ok", 200000000000+r.Int63n(7e11)) }},
		{"UK_NHS", func() string {
			return fmt.Sprintf("call %03d %03d %04d", 200+r.Intn(800), 200+r.Intn(800), r.Intn(10000))
		}},
		{"UK_NHS", func() string { return fmt.Sprintf("invoice %010d", r.Int63n(1e10)) }},
	} {
		if got := rate(t, dets, c.label, c.gen); got >= 1 {
			t.Errorf("%s false-positive rate %.1f%% (want < 1%%) on %q-style input", c.label, got, c.gen())
		}
	}
}

func TestGlobalSafeRecall(t *testing.T) {
	dets, _, _ := LoadMany([]string{"global-safe"})
	aadhaar := "234567890124" // Verhoeff-valid
	nhs := "9434765919"       // NHS Mod-11 valid
	for _, c := range []struct{ text, label string }{
		{"Customer Aadhaar 2345 6789 0124 asks about refunds", "AADHAAR"},
		{"my aadhar no is " + aadhaar, "AADHAAR"},
		{"2345-6789-0124", "AADHAAR"}, // formatted layout counts on its own
		{"UID: आधार " + aadhaar, "AADHAAR"},
		{"NHS number 943 476 5919", "UK_NHS"},
		{"patient no. " + nhs, "UK_NHS"},
	} {
		hs := dlp.Scan(c.text, dets)
		if len(hs) != 1 || hs[0].Label != c.label {
			t.Errorf("%q: got %+v, want one %s", c.text, hs, c.label)
		}
	}
	for _, text := range []string{"ref " + aadhaar + " done", "ticket 943 476 5919"} {
		if hs := dlp.Scan(text, dets); len(hs) != 0 {
			t.Errorf("%q: no keyword nearby, want no hit, got %+v", text, hs)
		}
	}
}
