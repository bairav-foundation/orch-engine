// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CacheDir returns the on-disk root for cached models.
//
// Resolution order:
//  1. $BAIRAV_CACHE_DIR if set (useful for tests, CI, and per-user overrides).
//  2. $HOME/.bairav/models otherwise.
func CacheDir() (string, error) {
	if v := os.Getenv("BAIRAV_CACHE_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".bairav", "models"), nil
}

// LocalPath returns the canonical on-disk path for a file pulled from a HF repo.
//
//	<CacheDir>/<org>/<repo>/<file-path-from-hf>
//
// The repo argument is the HF "<org>/<name>" slug; we use that as a two-segment
// path, which mirrors HF's own naming and makes `find` / `ls` natural.
//
// Both parts come from users or the Hub, so the result is checked to stay
// inside the cache: "../" in a repo or file name would otherwise let
// `pull` write, or `delete` remove, anything the user can.
func LocalPath(repo, file string) (string, error) {
	root, err := CacheDir()
	if err != nil {
		return "", err
	}
	org, name, ok := strings.Cut(repo, "/")
	if !ok || org == "" || name == "" || strings.Contains(name, "/") || badSegment(org) || badSegment(name) {
		return "", fmt.Errorf("invalid repo %q (want <org>/<name>)", repo)
	}
	if strings.HasPrefix(filepath.ToSlash(file), "/") {
		return "", fmt.Errorf("invalid file path %q in repo %s", file, repo)
	}
	for _, seg := range strings.Split(filepath.ToSlash(file), "/") {
		if seg == ".." {
			return "", fmt.Errorf("invalid file path %q in repo %s", file, repo)
		}
	}
	p := filepath.Join(root, org, name, filepath.FromSlash(file))
	if rel, err := filepath.Rel(root, p); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes the model cache", p)
	}
	return p, nil
}

func badSegment(s string) bool {
	return s == "." || s == ".." || strings.ContainsAny(s, "\\\x00")
}

// CachedModel describes one model directory inside the cache.
type CachedModel struct {
	// Repo is "<org>/<name>" reconstructed from the directory layout.
	Repo string
	// Files lists every regular file under the repo directory, with their byte size.
	Files []CachedFile
	// TotalSize is the sum of Files[].Size for convenience in `model list`.
	TotalSize int64
}

// CachedFile is one file inside a CachedModel.
type CachedFile struct {
	Path string
	Size int64
}

// ListCached walks the cache root and returns one entry per <org>/<repo> pair
// that holds at least one file. Sub-directories below the repo level are
// flattened into the Files list.
//
// Returns (nil, nil) if the cache root does not exist yet — that's the
// expected state on a fresh laptop, not an error.
func ListCached() ([]CachedModel, error) {
	root, err := CacheDir()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat cache root: %w", err)
	}

	// Cache layout: <root>/<org>/<repo>/<...files>.
	orgs, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read cache root: %w", err)
	}

	var out []CachedModel
	for _, org := range orgs {
		if !org.IsDir() {
			continue
		}
		orgPath := filepath.Join(root, org.Name())
		repos, err := os.ReadDir(orgPath)
		if err != nil {
			continue
		}
		for _, repo := range repos {
			if !repo.IsDir() {
				continue
			}
			repoPath := filepath.Join(orgPath, repo.Name())
			files, total, err := walkRepoFiles(repoPath)
			if err != nil || len(files) == 0 {
				continue
			}
			out = append(out, CachedModel{
				Repo:      org.Name() + "/" + repo.Name(),
				Files:     files,
				TotalSize: total,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out, nil
}

// walkRepoFiles enumerates every regular file under repoPath, returning paths
// relative to the repo root. Hidden files (starting with ".") and partial
// downloads (ending in ".partial") are skipped.
func walkRepoFiles(repoPath string) ([]CachedFile, int64, error) {
	var files []CachedFile
	var total int64
	err := filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		if base == "" || base[0] == '.' {
			return nil
		}
		if filepath.Ext(base) == ".partial" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(repoPath, path)
		if err != nil {
			return nil
		}
		files = append(files, CachedFile{
			Path: filepath.ToSlash(rel),
			Size: info.Size(),
		})
		total += info.Size()
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, total, nil
}

// FileSHA256 returns the hex-encoded SHA-256 of a file. Used by `model list`
// when the user wants to verify integrity. Streams in 64KB chunks so it works
// on multi-gigabyte models without buffering them in memory.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
