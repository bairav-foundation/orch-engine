// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embedded demo script must always parse — it ships in the binary and
// on bairavs.com, so a broken demo.json is a release blocker.
func TestDemoFramesValid(t *testing.T) {
	frames, err := DemoFrames()
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range frames {
		if f.Caption == "" || f.DurationMS <= 0 {
			t.Errorf("frame %d: caption %q, duration %d", i, f.Caption, f.DurationMS)
		}
		if f.Snapshot.Mode != "demo" {
			t.Errorf("frame %d: mode = %q, want demo", i, f.Snapshot.Mode)
		}
	}
}

func TestExportIsSelfContained(t *testing.T) {
	dir := t.TempDir()
	path, err := Export(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	if strings.Contains(page, framesPlaceholder) {
		t.Error("placeholder not replaced")
	}
	frames, _ := DemoFrames()
	// Captions are inlined via json.Marshal, so compare an escaped-safe prefix.
	if !strings.Contains(page, frames[0].Caption[:20]) {
		t.Error("first demo caption missing from export")
	}
	for _, ext := range []string{"src=\"http", "href=\"http://", "@import"} {
		if strings.Contains(page, ext) {
			t.Errorf("export references an external asset (%s); it must be one self-contained file", ext)
		}
	}
}

func TestRenderIndexEscapesScriptClose(t *testing.T) {
	page, err := renderIndex([]DemoFrame{{Caption: "</script><script>alert(1)</script>", DurationMS: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "</script><script>alert(1)") {
		t.Error("caption broke out of the inline <script> tag")
	}
}

func TestLiveHandler(t *testing.T) {
	h, err := Handler(Options{Snapshot: func(context.Context) Snapshot {
		return Snapshot{Mode: "live", Version: "vtest", Models: []Node{{Name: "qwen"}}}
	}})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/snapshot", nil))
	var s Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil || s.Version != "vtest" || len(s.Models) != 1 {
		t.Fatalf("snapshot = %+v, err %v", s, err)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("missing CSP header")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/snapshot", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST should be rejected (read-only), got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	page := rec.Body.String()
	// Regression: the live page shipped "/*BAIRAV_FRAMES*/null",
	// which JSON.parse rejects, so the UI never rendered. The frames
	// script tag must hold exactly valid JSON: null in live mode.
	if strings.Contains(page, framesPlaceholder) {
		t.Error("live page still contains the placeholder comment (invalid JSON in the browser)")
	}
	if !strings.Contains(page, `<script id="bairav-frames" type="application/json">null</script>`) {
		t.Error("live page should inline `null` frames so the UI polls /api/snapshot")
	}
}

func TestDemoHandlerHasNoAPI(t *testing.T) {
	h, err := Handler(Options{Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/snapshot", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("demo mode must not expose live data, got %d", rec.Code)
	}
}

func TestHandlerRequiresSnapshotInLiveMode(t *testing.T) {
	if _, err := Handler(Options{}); err == nil {
		t.Error("expected error without Snapshot func")
	}
}

func TestLoopbackOnly(t *testing.T) {
	h := LoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	for host, want := range map[string]int{
		"127.0.0.1:7777": 200, "localhost:7777": 200, "[::1]:7777": 200,
		"evil.example:7777": http.StatusMisdirectedRequest, "10.0.0.5:7777": http.StatusMisdirectedRequest,
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %s: got %d, want %d", host, w.Code, want)
		}
	}
}
