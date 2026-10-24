// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"context"
	"fmt"
	"regexp"
)

var (
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// IsCommit reports whether rev is already a full commit SHA.
func IsCommit(rev string) bool { return commitRE.MatchString(rev) }

// ResolveCommit turns a branch or tag ("main", "v1.0") into the commit SHA
// it points at right now. A full commit SHA is returned unchanged.
func (c *Client) ResolveCommit(ctx context.Context, repo, revision string) (string, error) {
	if revision == "" {
		revision = "main"
	}
	if IsCommit(revision) {
		return revision, nil
	}
	var info struct {
		SHA string `json:"sha"`
	}
	if err := c.getJSON(ctx, "/api/models/"+escapePathSegments(repo)+"/revision/"+escapePathSegments(revision), &info); err != nil {
		return "", fmt.Errorf("resolve %s@%s: %w", repo, revision, err)
	}
	if !IsCommit(info.SHA) {
		return "", fmt.Errorf("resolve %s@%s: Hugging Face returned no commit", repo, revision)
	}
	return info.SHA, nil
}

// Pinned is a model file fixed to one commit and one content hash.
type Pinned struct {
	Commit string // 40-hex commit SHA
	File   string // path inside the repo
	SHA256 string // 64-hex content hash
}

// Pin resolves revision to a commit and finds the SHA-256 of file at that
// commit. pick chooses the file when file is empty (the CLI's "smallest
// .gguf" rule); it returns nil when nothing fits.
func (c *Client) Pin(ctx context.Context, repo, revision, file string, pick func([]RepoFile) *RepoFile) (Pinned, error) {
	commit, err := c.ResolveCommit(ctx, repo, revision)
	if err != nil {
		return Pinned{}, err
	}
	files, err := c.ListFiles(ctx, repo, commit)
	if err != nil {
		return Pinned{}, err
	}
	var f *RepoFile
	if file == "" {
		f = pick(files)
		if f == nil {
			return Pinned{}, fmt.Errorf("pin %s: no suitable file at %s", repo, commit[:12])
		}
	} else {
		for i := range files {
			if files[i].Path == file {
				f = &files[i]
				break
			}
		}
		if f == nil {
			return Pinned{}, fmt.Errorf("pin %s: %s not found at %s", repo, file, commit[:12])
		}
	}
	if !sha256RE.MatchString(f.SHA256) {
		return Pinned{}, fmt.Errorf("pin %s: Hugging Face lists no SHA-256 for %s (not an LFS file?)", repo, f.Path)
	}
	return Pinned{Commit: commit, File: f.Path, SHA256: f.SHA256}, nil
}
