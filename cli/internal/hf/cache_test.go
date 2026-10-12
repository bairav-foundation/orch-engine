// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheDir_HonoursEnvOverride(t *testing.T) {
	t.Setenv("BAIRAV_CACHE_DIR", "/tmp/bairav-test-cache")
	got, err := CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/bairav-test-cache" {
		t.Errorf("CacheDir() = %q, want override", got)
	}
}

func TestCacheDir_DefaultsToHome(t *testing.T) {
	t.Setenv("BAIRAV_CACHE_DIR", "")
	got, err := CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, filepath.Join(".bairav", "models")) {
		t.Errorf("CacheDir() = %q, want suffix .bairav/models", got)
	}
}

func TestLocalPath(t *testing.T) {
	t.Setenv("BAIRAV_CACHE_DIR", "/tmp/cache")
	got, err := LocalPath("Qwen/Qwen2.5-0.5B-Instruct-GGUF", "model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	want := "/tmp/cache/Qwen/Qwen2.5-0.5B-Instruct-GGUF/model.gguf"
	if got != want {
		t.Errorf("LocalPath = %q, want %q", got, want)
	}
}

func TestListCached_EmptyCacheReturnsNil(t *testing.T) {
	t.Setenv("BAIRAV_CACHE_DIR", filepath.Join(t.TempDir(), "does-not-exist"))
	got, err := ListCached()
	if err != nil {
		t.Fatalf("expected nil error for missing cache, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result for missing cache, got %+v", got)
	}
}

func TestListCached_FindsPulledModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BAIRAV_CACHE_DIR", dir)

	// Lay down: <cache>/Qwen/Qwen2.5-0.5B/model.gguf  (1024 bytes)
	//          <cache>/Qwen/Qwen2.5-0.5B/config.json (200 bytes)
	//          <cache>/Qwen/Qwen2.5-0.5B/.partial-skip-me (should be skipped)
	//          <cache>/Mistral/Mistral-7B/q4.gguf    (2048 bytes)
	mustMkdirFile(t, dir, "Qwen/Qwen2.5-0.5B/model.gguf", 1024)
	mustMkdirFile(t, dir, "Qwen/Qwen2.5-0.5B/config.json", 200)
	mustMkdirFile(t, dir, "Qwen/Qwen2.5-0.5B/model.gguf.partial", 50)
	mustMkdirFile(t, dir, "Mistral/Mistral-7B/q4.gguf", 2048)

	got, err := ListCached()
	if err != nil {
		t.Fatalf("ListCached: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 cached models, got %d: %+v", len(got), got)
	}

	// Sorted alphabetically.
	if got[0].Repo != "Mistral/Mistral-7B" {
		t.Errorf("first repo = %q, want Mistral/Mistral-7B", got[0].Repo)
	}
	if got[0].TotalSize != 2048 {
		t.Errorf("Mistral total = %d, want 2048", got[0].TotalSize)
	}

	if got[1].Repo != "Qwen/Qwen2.5-0.5B" {
		t.Errorf("second repo = %q, want Qwen/Qwen2.5-0.5B", got[1].Repo)
	}
	// .partial file must be excluded.
	if got[1].TotalSize != 1224 {
		t.Errorf("Qwen total = %d, want 1224 (excluding .partial)", got[1].TotalSize)
	}
	if len(got[1].Files) != 2 {
		t.Errorf("Qwen file count = %d, want 2 (excluding .partial)", len(got[1].Files))
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{int64(1.5 * 1024 * 1024), "1.5 MiB"},
	}
	for _, c := range cases {
		got := HumanBytes(c.in)
		if got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// mustMkdirFile creates dir + writes a zero-padded file of the given size.
func mustMkdirFile(t *testing.T, root, rel string, size int) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, size)
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLocalPathRejectsTraversal(t *testing.T) {
	t.Setenv("BAIRAV_CACHE_DIR", t.TempDir())
	for _, c := range [][2]string{
		{"../..", ""}, {"Qwen/..", ""}, {"Qwen", "x.gguf"}, {"a/b/c", ""},
		{"Qwen/x", "../../.ssh/id_rsa"}, {"Qwen/x", "/etc/passwd"},
	} {
		if p, err := LocalPath(c[0], c[1]); err == nil {
			t.Errorf("LocalPath(%q, %q) = %q, want an error", c[0], c[1], p)
		}
	}
	if _, err := LocalPath("Qwen/x", "sub/dir/model.gguf"); err != nil {
		t.Errorf("nested file inside the repo must be allowed: %v", err)
	}
}
