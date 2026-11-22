// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

// PDFs for attachments: scan, read inside the OCR container.
//
// A PDF is either text (a statement exported from a system: the text is
// in the file) or scanned (each page is a picture). We read the text layer
// with poppler's pdftotext, and every page that has (almost) no text is
// rendered with pdftoppm and read with Tesseract, like a screenshot. Both
// tools run on the untrusted file inside the dlp-ocr container, not in the
// proxy, with page and time limits; anything over a limit fails, and the
// proxy then blocks the PDF (fail closed).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// maxPDFPages is the most pages a PDF may have; longer ones are refused
	// (and blocked by the proxy) rather than half-read.
	maxPDFPages = 50
	// maxScannedPages is how many pages without a text layer are OCRed.
	// A 40-page scan takes minutes; that's not a chat request.
	maxScannedPages = 15
	// pdfTimeout bounds one PDF, all pages.
	pdfTimeout = 90 * time.Second
	// minPageText is how many characters make a page "text": fewer means
	// it is a scan (or a cover image) and gets OCR.
	minPageText = 25
)

// pdfTools are the poppler binaries the PDF path may run.
var pdfTools = map[string]bool{"pdfinfo": true, "pdftotext": true, "pdftoppm": true}

func runPDFTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	if !pdfTools[name] {
		return nil, fmt.Errorf("not a PDF tool: %s", name)
	}
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // allowlisted binary; args are fixed flags and paths we created
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%s not found: install poppler (brew install poppler, apt install poppler-utils) or use the dlp-ocr image", name)
		}
		var xe *exec.ExitError
		if errors.As(err, &xe) {
			return nil, fmt.Errorf("%s: %v: %s", name, err, clipText(string(xe.Stderr), 200))
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

var pagesRe = regexp.MustCompile(`(?m)^Pages:\s+(\d+)`)

// pdfText returns the text of a PDF: its text layer, plus OCR of pages
// that have none.
func (s *OCRServer) pdfText(ctx context.Context, pdf []byte) (string, error) {
	tool := s.Tool
	if tool == nil {
		tool = runPDFTool
	}
	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "bairav-pdf-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, pdf, 0o600); err != nil {
		return "", err
	}

	info, err := tool(ctx, "pdfinfo", in)
	if err != nil {
		return "", err // encrypted, damaged or not a PDF
	}
	m := pagesRe.FindSubmatch(info)
	if m == nil {
		return "", errors.New("pdfinfo: no page count")
	}
	pages, _ := strconv.Atoi(string(m[1]))
	if pages < 1 || pages > maxPDFPages {
		return "", fmt.Errorf("PDF has %d pages; the limit is %d", pages, maxPDFPages)
	}

	layer, err := tool(ctx, "pdftotext", "-layout", "-enc", "UTF-8", in, "-")
	if err != nil {
		return "", err
	}
	// pdftotext ends every page with a form feed.
	texts := strings.Split(string(layer), "\f")

	pageText := func(p int) string {
		if p-1 < len(texts) {
			return texts[p-1]
		}
		return ""
	}
	// Count the pages without text first, so a long scan is refused
	// before any rendering.
	scanned := 0
	for p := 1; p <= pages; p++ {
		if len(strings.TrimSpace(pageText(p))) < minPageText {
			scanned++
		}
	}
	if scanned > maxScannedPages {
		return "", fmt.Errorf("%d scanned pages; the limit is %d", scanned, maxScannedPages)
	}

	var b strings.Builder
	for p := 1; p <= pages; p++ {
		t := pageText(p)
		b.WriteString(t)
		b.WriteString("\n")
		if len(strings.TrimSpace(t)) >= minPageText {
			continue
		}
		ocr, err := s.ocrPDFPage(ctx, tool, in, dir, p)
		if err != nil {
			return "", fmt.Errorf("page %d: %w", p, err)
		}
		b.WriteString(ocr)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// ocrPDFPage renders one page at 200 dpi and reads it with Tesseract.
func (s *OCRServer) ocrPDFPage(ctx context.Context, tool func(context.Context, string, ...string) ([]byte, error), in, dir string, page int) (string, error) {
	prefix := filepath.Join(dir, "page")
	n := strconv.Itoa(page)
	if _, err := tool(ctx, "pdftoppm", "-r", "200", "-png", "-singlefile", "-f", n, "-l", n,
		"-scale-to", "4000", in, prefix); err != nil {
		return "", err
	}
	img, err := os.ReadFile(prefix + ".png")
	if err != nil {
		return "", err
	}
	_ = os.Remove(prefix + ".png")
	out, err := s.Run(ctx, prepareForOCR(img), "stdin", "stdout", "-l", s.Langs, "--psm", "3")
	if err != nil {
		return "", err
	}
	return string(out), nil
}
