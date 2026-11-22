// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// fakeOCR returns canned text per image (keyed by the image bytes).
type fakeOCR map[string]string

func (f fakeOCR) Text(_ context.Context, _ string, img []byte) (string, error) {
	if t, ok := f[string(img)]; ok {
		return t, nil
	}
	return "", errors.New("unreadable")
}

func imageChatWith(data []byte) string {
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	return `{"messages":[{"role":"user","content":[{"type":"text","text":"what does this say"},{"type":"image_url","image_url":{"url":"` + uri + `"}}]}]}`
}

func scanProxy(t *testing.T, ocr OCR, extra ...Detector) (*httptest.Server, *recordingUpstream, *collectingAudit) {
	t.Helper()
	up := newRecordingUpstream()
	audit := newCollectingAudit()
	cfg := Config{Upstream: up.srv.URL + "/v1", Audit: audit, Attachments: AttachmentsScan, OCR: ocr}
	if extra != nil {
		cfg.Detectors = append(DefaultDetectors(), extra...)
	}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(func() { srv.Close(); up.close() })
	return srv, up, audit
}

func TestScan_BlocksImageWithSensitiveText(t *testing.T) {
	ocr := fakeOCR{"card": "Card number 4111 1111 1111 1111 exp 12/29", "clean": "Weekly sales report"}
	srv, up, audit := scanProxy(t, ocr)

	resp := postChat(t, srv.URL, imageChatWith([]byte("card")))
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "dlp_image_blocked") || !strings.Contains(string(b), "CREDIT_CARD") {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	e := audit.next(t)
	if e.Action != ActionBlock || !reflect.DeepEqual(e.ImageLabels, []string{"CREDIT_CARD"}) || !reflect.DeepEqual(e.Scanned, []string{"image"}) {
		t.Fatalf("audit: %+v", e)
	}
	line, _ := json.Marshal(e)
	if strings.Contains(string(line), "4111") || strings.Contains(string(line), "Card number") {
		t.Fatalf("audit holds OCR text or the raw match: %s", line)
	}
	up.mu.Lock()
	got := up.gotBody
	up.mu.Unlock()
	if got != nil {
		t.Fatal("blocked image reached the upstream")
	}

	// A clean image passes, marked as scanned.
	resp = postChat(t, srv.URL, imageChatWith([]byte("clean")))
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get(ScannedHeader) != "image" || resp.Header.Get(UninspectedHeader) != "" {
		t.Fatalf("clean image: %d, headers %v", resp.StatusCode, resp.Header)
	}
	if e := audit.next(t); len(e.Uninspected) != 0 || e.Action == ActionBlock {
		t.Fatalf("clean audit: %+v", e)
	}
}

func TestScan_FailsClosed(t *testing.T) {
	srv, _, audit := scanProxy(t, fakeOCR{})
	for name, body := range map[string]string{
		"ocr error":  imageChatWith([]byte("garbage")),
		"image link": `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/id.png"}}]}]}`,
		"pdf":        `{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}}]}]}`,
	} {
		resp := postChat(t, srv.URL, body)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "dlp_attachment_blocked") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
		if e := audit.next(t); e.Action != ActionBlock || len(e.Uninspected) == 0 {
			t.Errorf("%s audit: %+v", name, e)
		}
	}
	if _, err := NewProxy(Config{Upstream: "http://x/v1", Attachments: AttachmentsScan}); err == nil {
		t.Error("scan without an OCR server must not start")
	}
}

// End to end with real Tesseract: the OCR server over HTTP, the proxy in
// scan mode, a fake card screenshot from the test set.
func TestScan_RealTesseract(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("tesseract not installed")
	}
	ocrSrv := httptest.NewServer(NewOCRServer("eng"))
	defer ocrSrv.Close()
	srv, _, audit := scanProxy(t, NewHTTPOCR(ocrSrv.URL))
	for file, wantBlocked := range map[string]bool{"card-checkout--plain.png": true, "clean-report--plain.png": false} {
		img, err := os.ReadFile(filepath.Join("testdata", "ocr", file))
		if err != nil {
			t.Fatal(err)
		}
		resp := postChat(t, srv.URL, imageChatWith(img))
		resp.Body.Close()
		if blocked := resp.StatusCode == http.StatusForbidden; blocked != wantBlocked {
			t.Errorf("%s: status %d, want blocked=%v", file, resp.StatusCode, wantBlocked)
		}
		audit.next(t)
	}
}

func TestOCRServer_Contract(t *testing.T) {
	s := &OCRServer{Langs: "eng", Run: func(_ context.Context, in []byte, args ...string) ([]byte, error) {
		return []byte("read: " + string(in)), nil
	}}
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/ocr", "image/png", strings.NewReader("abc"))
	var r ocrReply
	_ = json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if resp.StatusCode != 200 || r.Text != "read: abc" {
		t.Fatalf("ocr: %d %+v", resp.StatusCode, r)
	}
	resp, _ = http.Post(srv.URL+"/ocr", "application/zip", strings.NewReader("abc"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("zip: %d", resp.StatusCode)
	}
	// A tiny PNG header claiming 60000x60000 pixels is refused unread.
	bomb := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\xea\x60\x00\x00\xea\x60\x08\x02\x00\x00\x00\x0f\xb0\xe2\x15")
	resp, _ = http.Post(srv.URL+"/ocr", "image/png", bytes.NewReader(bomb))
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("pixel bomb: %d", resp.StatusCode)
	}
	if !validLangs("eng+hin") || validLangs("eng;rm") || validLangs("") {
		t.Fatal("validLangs")
	}
	text, err := NewHTTPOCR(srv.URL).Text(context.Background(), "image/png", []byte("xyz"))
	if err != nil || text != "read: xyz" {
		t.Fatalf("client: %q %v", text, err)
	}
}
