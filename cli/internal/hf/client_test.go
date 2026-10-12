// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeHFListResponse mimics the JSON shape of /api/models/<repo>/tree/<rev>?recursive=true.
const fakeHFListResponse = `[
  {"path": "README.md", "type": "file", "size": 1234, "oid": "abc123"},
  {"path": "config.json", "type": "file", "size": 567, "oid": "def456"},
  {"path": "qwen2.5-0.5b-instruct-q4_k_m.gguf", "type": "file", "size": 397000000, "oid": "deadbeef"},
  {"path": "subdir", "type": "directory", "size": 0, "oid": ""}
]`

func TestListFiles_Happy(t *testing.T) {
	var gotPath, gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeHFListResponse))
	}))
	defer srv.Close()

	c := &Client{
		Endpoint:   srv.URL,
		HTTPClient: srv.Client(),
		Token:      "test-token",
	}

	files, err := c.ListFiles(context.Background(), "Qwen/Qwen2.5-0.5B-Instruct-GGUF", "")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}

	// Directory entry must be filtered out.
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %+v", len(files), files)
	}
	if files[2].Path != "qwen2.5-0.5b-instruct-q4_k_m.gguf" {
		t.Errorf("expected gguf file, got %q", files[2].Path)
	}
	if files[2].Size != 397_000_000 {
		t.Errorf("expected size 397000000, got %d", files[2].Size)
	}
	if files[2].SHA != "deadbeef" {
		t.Errorf("expected sha 'deadbeef', got %q", files[2].SHA)
	}

	// Auth + UA headers should be set.
	if gotAuth != "Bearer test-token" {
		t.Errorf("expected bearer auth, got %q", gotAuth)
	}
	if !strings.HasPrefix(gotUA, "bairav/") {
		t.Errorf("expected bairav/ user agent, got %q", gotUA)
	}

	// Path should default revision to "main" and use literal slashes between
	// org and repo — not "%2F" (that breaks the Hub API).
	wantPath := "/api/models/Qwen/Qwen2.5-0.5B-Instruct-GGUF/tree/main"
	if gotPath != wantPath {
		t.Errorf("request Path = %q, want %q", gotPath, wantPath)
	}
	if strings.Contains(gotPath, "%2F") {
		t.Errorf("repo slug must not be url-escaped as a single segment (got %%2F in %q)", gotPath)
	}
}

func TestListFiles_AnonymousNoAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, HTTPClient: srv.Client(), Token: ""}
	if _, err := c.ListFiles(context.Background(), "test/repo", ""); err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("expected no auth header for anonymous client, got %q", gotAuth)
	}
}

func TestListFiles_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, HTTPClient: srv.Client()}
	_, err := c.ListFiles(context.Background(), "nope/missing", "")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected error to mention 'not found', got %q", err.Error())
	}
}

func TestListFiles_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, HTTPClient: srv.Client()}
	_, err := c.ListFiles(context.Background(), "private/repo", "")
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	if !strings.Contains(err.Error(), "HF_TOKEN") {
		t.Errorf("expected error to mention HF_TOKEN, got %q", err.Error())
	}
}

func TestBlobURL(t *testing.T) {
	c := &Client{Endpoint: "https://example.test"}
	got := c.BlobURL("Qwen/Qwen2.5-0.5B-Instruct-GGUF", "main", "model.gguf")
	want := "https://example.test/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/model.gguf"
	if got != want {
		t.Errorf("BlobURL =\n  %s\nwant\n  %s", got, want)
	}
}

func TestBlobURL_DefaultRevision(t *testing.T) {
	c := &Client{Endpoint: "https://example.test"}
	got := c.BlobURL("foo/bar", "", "x.bin")
	if !strings.Contains(got, "/resolve/main/") {
		t.Errorf("expected default revision 'main' in URL, got %q", got)
	}
}

func TestSearchModels(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"id": "bartowski/Qwen2.5-0.5B-Instruct-GGUF", "downloads": 12345, "likes": 56},
		  {"id": "Qwen/Qwen2.5-0.5B-Instruct-GGUF", "downloads": 9000, "likes": 80}
		]`))
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, HTTPClient: srv.Client()}
	hits, err := c.SearchModels(context.Background(), "Qwen2.5-0.5B-Instruct", true, 5)
	if err != nil {
		t.Fatalf("SearchModels: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].ID != "bartowski/Qwen2.5-0.5B-Instruct-GGUF" {
		t.Errorf("first hit = %q, want bartowski/Qwen2.5-0.5B-Instruct-GGUF", hits[0].ID)
	}
	// Verify the GGUF filter and limit made it to the request.
	if !strings.Contains(gotURL, "filter=gguf") {
		t.Errorf("expected filter=gguf in URL, got %q", gotURL)
	}
	if !strings.Contains(gotURL, "limit=5") {
		t.Errorf("expected limit=5 in URL, got %q", gotURL)
	}
}
