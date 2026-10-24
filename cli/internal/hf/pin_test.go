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

const (
	testCommit = "9217f5db79a29953eb74d5343926648285ec7e67"
	testSHA256 = "74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db"
)

// fakeHub answers the two calls Pin makes, shaped like huggingface.co.
func fakeHub(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.Path)
		switch r.URL.Path {
		case "/api/models/Qwen/Qwen2.5-0.5B-Instruct-GGUF/revision/main":
			_, _ = w.Write([]byte(`{"id":"Qwen/Qwen2.5-0.5B-Instruct-GGUF","sha":"` + testCommit + `","siblings":[]}`))
		case "/api/models/Qwen/Qwen2.5-0.5B-Instruct-GGUF/tree/" + testCommit:
			_, _ = w.Write([]byte(`[
				{"type":"file","oid":"1111111111111111111111111111111111111111","size":1519,"path":"README.md"},
				{"type":"file","oid":"2222222222222222222222222222222222222222","size":491400032,"path":"qwen2.5-0.5b-instruct-q4_k_m.gguf",
				 "lfs":{"oid":"` + testSHA256 + `","size":491400032,"pointerSize":135}},
				{"type":"file","oid":"3333333333333333333333333333333333333333","size":990000000,"path":"qwen2.5-0.5b-instruct-fp16.gguf",
				 "lfs":{"oid":"` + strings.Repeat("b", 64) + `","size":990000000,"pointerSize":135}}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func smallest(files []RepoFile) *RepoFile {
	var best *RepoFile
	for i := range files {
		if strings.HasSuffix(files[i].Path, ".gguf") && (best == nil || files[i].Size < best.Size) {
			best = &files[i]
		}
	}
	return best
}

func TestPinResolvesCommitAndHash(t *testing.T) {
	var seen []string
	srv := fakeHub(t, &seen)
	c := NewClient()
	c.Endpoint = srv.URL
	p, err := c.Pin(context.Background(), "Qwen/Qwen2.5-0.5B-Instruct-GGUF", "main", "", smallest)
	if err != nil {
		t.Fatal(err)
	}
	if p.Commit != testCommit || p.File != "qwen2.5-0.5b-instruct-q4_k_m.gguf" || p.SHA256 != testSHA256 {
		t.Errorf("pin = %+v", p)
	}
	// The file list must come from the commit, not from main (which can move).
	if len(seen) != 2 || !strings.HasSuffix(seen[1], "/tree/"+testCommit) {
		t.Errorf("requests = %v; the tree must be read at the resolved commit", seen)
	}
}

func TestPinExplicitFileAndErrors(t *testing.T) {
	var seen []string
	srv := fakeHub(t, &seen)
	c := NewClient()
	c.Endpoint = srv.URL
	ctx := context.Background()
	p, err := c.Pin(ctx, "Qwen/Qwen2.5-0.5B-Instruct-GGUF", "main", "qwen2.5-0.5b-instruct-fp16.gguf", smallest)
	if err != nil || p.SHA256 != strings.Repeat("b", 64) {
		t.Errorf("explicit file: %+v %v", p, err)
	}
	if _, err := c.Pin(ctx, "Qwen/Qwen2.5-0.5B-Instruct-GGUF", "main", "missing.gguf", smallest); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing file: %v", err)
	}
	if _, err := c.Pin(ctx, "Qwen/Qwen2.5-0.5B-Instruct-GGUF", "main", "README.md", smallest); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("non-LFS file: %v", err)
	}
	// A commit SHA is used as is, with no revision lookup.
	seen = nil
	if _, err := c.Pin(ctx, "Qwen/Qwen2.5-0.5B-Instruct-GGUF", testCommit, "", smallest); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Errorf("commit revision should skip the lookup, requests = %v", seen)
	}
}
