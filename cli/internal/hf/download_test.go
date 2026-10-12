// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// rangeServer returns a handler that serves a fixed payload, honouring
// "Range: bytes=N-" headers with HTTP 206 responses.
func rangeServer(payload []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			rangeHdr := r.Header.Get("Range")
			if rangeHdr == "" {
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				_, _ = w.Write(payload)
				return
			}
			// Parse "bytes=N-".
			var start int
			_, _ = fmt.Sscanf(rangeHdr, "bytes=%d-", &start)
			if start >= len(payload) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[start:])
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func TestDownload_FullFreshDownload(t *testing.T) {
	payload := []byte(strings.Repeat("hello world\n", 100)) // 1200 bytes
	srv := httptest.NewServer(rangeServer(payload))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "out.bin")
	c := &Client{Endpoint: srv.URL, HTTPClient: srv.Client()}

	var lastReported int64
	err := c.Download(context.Background(), srv.URL+"/file", dst, func(d, total int64) {
		lastReported = d
		if total != int64(len(payload)) {
			t.Errorf("progress total = %d, want %d", total, len(payload))
		}
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if lastReported != int64(len(payload)) {
		t.Errorf("final progress = %d, want %d", lastReported, len(payload))
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("file contents mismatch")
	}

	// .partial sidecar should be cleaned up.
	if _, err := os.Stat(dst + ".partial"); !os.IsNotExist(err) {
		t.Errorf(".partial sidecar should be gone after success")
	}
}

func TestDownload_NoOpWhenComplete(t *testing.T) {
	payload := []byte("complete file contents")
	srv := httptest.NewServer(rangeServer(payload))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "done.bin")
	if err := os.WriteFile(dst, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	requestCount := 0
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		rangeServer(payload).ServeHTTP(w, r)
	})
	srv2 := httptest.NewServer(wrapped)
	defer srv2.Close()

	c := &Client{Endpoint: srv2.URL, HTTPClient: srv2.Client()}
	if err := c.Download(context.Background(), srv2.URL+"/file", dst, nil); err != nil {
		t.Fatalf("Download (no-op): %v", err)
	}
	// We expect exactly 1 request (the HEAD probe) — no GET.
	if requestCount != 1 {
		t.Errorf("expected 1 HTTP request (HEAD only), got %d", requestCount)
	}
}

func TestDownload_ResumesFromPartial(t *testing.T) {
	payload := []byte(strings.Repeat("0123456789", 200)) // 2000 bytes
	srv := httptest.NewServer(rangeServer(payload))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "resumeme.bin")
	partial := dst + ".partial"

	// Pre-seed a .partial with the first 800 bytes.
	if err := os.WriteFile(partial, payload[:800], 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Client{Endpoint: srv.URL, HTTPClient: srv.Client()}
	err := c.Download(context.Background(), srv.URL+"/file", dst, nil)
	if err != nil {
		t.Fatalf("Download (resume): %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("resumed file contents mismatch — len got=%d want=%d",
			len(got), len(payload))
	}
}
