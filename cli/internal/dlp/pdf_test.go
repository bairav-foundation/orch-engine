// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func havePDFTools(t *testing.T) {
	t.Helper()
	for _, b := range []string{"tesseract", "pdfinfo", "pdftotext", "pdftoppm"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip(b + " not installed")
		}
	}
}

// TestPDFTestSet runs real poppler + Tesseract over testdata/pdf (made by
// scripts/gen-pdf-testset.py): text PDFs, scanned PDFs, multi-page and
// mixed ones. Every sensitive PDF must be caught, no clean one flagged.
func TestPDFTestSet(t *testing.T) {
	havePDFTools(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "pdf", "manifest.json"))
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
	s := NewOCRServer("eng")
	found, want := 0, 0
	for _, c := range cases {
		pdf, err := os.ReadFile(filepath.Join("testdata", "pdf", c.File))
		if err != nil {
			t.Fatal(err)
		}
		text, err := s.pdfText(context.Background(), pdf)
		if err != nil {
			t.Errorf("%s: %v", c.File, err)
			continue
		}
		got := Labels(Scan(text, dets))
		if len(c.Labels) == 0 {
			if len(got) > 0 {
				t.Errorf("FALSE POSITIVE %s: %v", c.File, got)
			}
			continue
		}
		want++
		if hasAll(got, c.Labels) {
			found++
		} else {
			t.Errorf("MISSED %s: want %v, got %v; read %q", c.File, c.Labels, got, clipText(text, 160))
		}
	}
	t.Logf("PDFs: found %d/%d with sensitive data", found, want)
}

func TestPDFLimitsAndErrors(t *testing.T) {
	s := &OCRServer{Langs: "eng", Run: func(context.Context, []byte, ...string) ([]byte, error) { return []byte("ocr"), nil }}
	for name, info := range map[string]string{"too many pages": "Pages:          51\n", "no count": "Title: x\n"} {
		info := info
		s.Tool = func(_ context.Context, tool string, _ ...string) ([]byte, error) {
			if tool == "pdfinfo" {
				return []byte(info), nil
			}
			return []byte("text"), nil
		}
		if _, err := s.pdfText(context.Background(), []byte("%PDF-1.4")); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// 20 empty pages = 20 scans, over the scanned-page limit.
	s.Tool = func(_ context.Context, tool string, _ ...string) ([]byte, error) {
		if tool == "pdfinfo" {
			return []byte("Pages: 20\n"), nil
		}
		return []byte(strings.Repeat("\f", 20)), nil
	}
	if _, err := s.pdfText(context.Background(), []byte("%PDF-1.4")); err == nil || !strings.Contains(err.Error(), "scanned pages") {
		t.Errorf("scanned-page limit: %v", err)
	}
}

func TestPDFAttachmentShapes(t *testing.T) {
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake"))
	for name, body := range map[string]string{
		"openai chat file":     `{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"s.pdf","file_data":"data:application/pdf;base64,` + pdf + `"}}]}]}`,
		"responses input_file": `{"input":[{"role":"user","content":[{"type":"input_file","filename":"s.pdf","file_data":"data:application/pdf;base64,` + pdf + `"}]}]}`,
		"anthropic document":   `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + pdf + `"}}]}]}`,
		"raw base64, no name":  `{"messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"` + pdf + `"}}]}]}`,
	} {
		var v interface{}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		as := collectAttachments(v)
		if len(as) != 1 || as[0].Kind != KindPDF || string(as[0].Data[:5]) != "%PDF-" {
			t.Errorf("%s: %+v", name, as)
		}
	}
}

// End to end: a scanned PDF with a key, and a clean text PDF, through the
// proxy in scan mode and a real OCR server.
func TestScan_PDFEndToEnd(t *testing.T) {
	havePDFTools(t)
	ocrSrv := httptest.NewServer(NewOCRServer("eng"))
	defer ocrSrv.Close()
	srv, _, audit := scanProxy(t, NewHTTPOCR(ocrSrv.URL))
	for file, wantBlocked := range map[string]bool{"scan-aws.pdf": true, "clean-report.pdf": false} {
		pdf, err := os.ReadFile(filepath.Join("testdata", "pdf", file))
		if err != nil {
			t.Fatal(err)
		}
		body := `{"messages":[{"role":"user","content":[{"type":"text","text":"summarise"},{"type":"file","file":{"filename":"` + file + `","file_data":"data:application/pdf;base64,` + base64.StdEncoding.EncodeToString(pdf) + `"}}]}]}`
		resp := postChat(t, srv.URL, body)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		blocked := resp.StatusCode == http.StatusForbidden
		if blocked != wantBlocked {
			t.Errorf("%s: %d %s", file, resp.StatusCode, b)
		}
		e := audit.next(t)
		if !reflect.DeepEqual(e.Scanned, []string{"pdf"}) {
			t.Errorf("%s audit scanned=%v uninspected=%v", file, e.Scanned, e.Uninspected)
		}
		if blocked && !strings.Contains(string(b), "AWS_ACCESS_KEY") {
			t.Errorf("%s: block reason: %s", file, b)
		}
	}
}
