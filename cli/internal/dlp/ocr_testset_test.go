// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestOCRTestSet runs real Tesseract over the fake-data test set in
// testdata/ocr (made by scripts/gen-ocr-testset.py) and checks what the
// detectors find in each image. It's the number behind the docs' detection
// rate. Skipped where tesseract isn't installed; the nightly installs it.
//
//	go test ./internal/dlp/ -run OCRTestSet -v     # per-image table
func TestOCRTestSet(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("tesseract not installed")
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "ocr", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		File   string   `json:"file"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	dets, err := detectorsWithPack("global-safe")
	if err != nil {
		t.Fatal(err)
	}
	ocr := NewOCRServer("eng")

	var found, want, falsePos, clean int
	byVariant := map[string][2]int{}
	for _, c := range cases {
		img, err := os.ReadFile(filepath.Join("testdata", "ocr", c.File))
		if err != nil {
			t.Fatal(err)
		}
		out, err := ocr.Run(context.Background(), prepareForOCR(img), "stdin", "stdout", "-l", "eng", "--psm", "3")
		if err != nil {
			t.Fatalf("%s: %v", c.File, err)
		}
		got := Labels(Scan(string(out), dets))
		variant := c.File[strings.Index(c.File, "--")+2 : strings.LastIndex(c.File, ".")]
		v := byVariant[variant]
		if len(c.Labels) == 0 {
			clean++
			if len(got) > 0 {
				falsePos++
				t.Logf("FALSE POSITIVE %-32s found %v", c.File, got)
			}
			continue
		}
		want++
		v[1]++
		if hasAll(got, c.Labels) {
			found++
			v[0]++
		} else {
			t.Logf("MISSED         %-32s want %v, got %v; OCR read %q", c.File, c.Labels, got, clipText(string(out), 120))
		}
		byVariant[variant] = v
	}
	var names []string
	for k := range byVariant {
		names = append(names, k)
	}
	sort.Strings(names)
	var parts []string
	for _, k := range names {
		parts = append(parts, fmt.Sprintf("%s %d/%d", k, byVariant[k][0], byVariant[k][1]))
	}
	t.Logf("detected %d/%d images with sensitive data (%.0f%%); false positives %d/%d clean images", found, want, 100*float64(found)/float64(want), falsePos, clean)
	t.Logf("by variant: %s", strings.Join(parts, ", "))

	// Floors, not targets: catch a regression in OCR or detectors. The
	// published rate is whatever this test prints.
	if float64(found) < 0.8*float64(want) {
		t.Errorf("detection rate below 80%%: %d/%d", found, want)
	}
	if falsePos > 0 {
		t.Errorf("%d false positives on clean images", falsePos)
	}
}

func hasAll(got, want []string) bool {
	for _, w := range want {
		ok := false
		for _, g := range got {
			if g == w {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// detectorsWithPack is DefaultDetectors plus a built-in pack, read from
// its YAML (the packs package imports this one, so it can't be used here).
func detectorsWithPack(name string) ([]Detector, error) {
	b, err := os.ReadFile(filepath.Join("packs", name+".yaml"))
	if err != nil {
		return nil, err
	}
	extra, _, err := ParsePolicy(b, name)
	if err != nil {
		return nil, err
	}
	return append(DefaultDetectors(), extra...), nil
}
