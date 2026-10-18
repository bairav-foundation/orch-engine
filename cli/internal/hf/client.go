// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// DefaultEndpoint is the public HuggingFace Hub URL.
	DefaultEndpoint = "https://huggingface.co"

	// UserAgent is the value sent in the User-Agent header on every request.
	// HF rate-limits by UA + IP, so we identify ourselves cleanly.
	UserAgent = "bairav/cli (+https://bairavs.com)"
)

// Client talks to the HuggingFace Hub HTTP API.
//
// Construct with NewClient(); fields can be overridden in tests.
type Client struct {
	Endpoint   string
	HTTPClient *http.Client
	// Token is an optional HF access token. If empty, NewClient reads HF_TOKEN
	// from the environment. Anonymous requests work for public repos.
	Token string
}

// NewClient returns a Client wired to the public HF endpoint with HF_TOKEN
// auto-loaded from the environment (if present).
//
// The HTTP client uses no timeout because model downloads can legitimately
// take many minutes. Callers control cancellation via context.Context.
func NewClient() *Client {
	return &Client{
		Endpoint: DefaultEndpoint,
		HTTPClient: &http.Client{
			Timeout: 0,
		},
		Token: os.Getenv("HF_TOKEN"),
	}
}

// RepoFile describes one file in a HuggingFace repository.
type RepoFile struct {
	// Path is the file path inside the repo, e.g. "qwen2.5-0.5b-instruct-q4_k_m.gguf".
	Path string
	// Size is the byte length of the file.
	Size int64
	// SHA is the git OID (blob hash) of the file. Useful for integrity checks.
	SHA string
}

// repoTreeEntry is the JSON shape returned by /api/models/<repo>/tree/<rev>.
type repoTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	OID  string `json:"oid"`
}

// escapePathSegments applies url.PathEscape to each slash-separated segment and
// rejoins with "/". Hugging Face paths are multi-segment (e.g. org/repo); escaping
// the whole string turns "/" into "%2F", which the Hub rejects ("url-encoded slash").
func escapePathSegments(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// ListFiles returns every blob (file) reachable from the given revision.
// repo is "<org>/<name>" e.g. "Qwen/Qwen2.5-0.5B-Instruct-GGUF".
// revision defaults to "main" when empty.
//
// Subdirectories are traversed (?recursive=true), so callers get a flat list.
func (c *Client) ListFiles(ctx context.Context, repo, revision string) ([]RepoFile, error) {
	if revision == "" {
		revision = "main"
	}
	u := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=true",
		strings.TrimRight(c.Endpoint, "/"),
		escapePathSegments(repo),
		escapePathSegments(revision),
	)

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.applyAuth(req)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hf list %s: %w", repo, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("hf list %s: %s — set HF_TOKEN if this repo is gated/private",
			repo, resp.Status)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("hf list %s: not found — check the repo name and revision", repo)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("hf list %s: %s — %s",
			repo, resp.Status, strings.TrimSpace(string(body)))
	}

	var entries []repoTreeEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode hf list response: %w", err)
	}

	out := make([]RepoFile, 0, len(entries))
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		out = append(out, RepoFile{Path: e.Path, Size: e.Size, SHA: e.OID})
	}
	return out, nil
}

// BlobURL returns the resolved download URL for a single file in a repo.
// HF redirects this URL to the actual CDN (cdn-lfs.huggingface.co for LFS files),
// and Go's default http.Client follows the redirect transparently.
func (c *Client) BlobURL(repo, revision, path string) string {
	if revision == "" {
		revision = "main"
	}
	return fmt.Sprintf("%s/%s/resolve/%s/%s",
		strings.TrimRight(c.Endpoint, "/"),
		repo,
		revision,
		path,
	)
}

// applyAuth sets the User-Agent and (if a token is configured) the bearer auth.
func (c *Client) applyAuth(req *http.Request) {
	req.Header.Set("User-Agent", UserAgent)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// SearchHit is one model returned by /api/models.
type SearchHit struct {
	ID        string `json:"id"`
	Downloads int64  `json:"downloads"`
	Likes     int    `json:"likes"`
}

// SearchModels queries HuggingFace's model search. If ggufOnly is true, filters
// to repos containing GGUF files. limit caps results (defaults to 10 if 0).
//
// Uses a tight 5s timeout because callers typically invoke this from error
// paths where a slow response would itself become a UX problem. Returns the
// network error so callers can decide whether to surface or swallow it.
func (c *Client) SearchModels(ctx context.Context, query string, ggufOnly bool, limit int) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 10
	}
	params := url.Values{}
	params.Set("search", query)
	params.Set("limit", fmt.Sprintf("%d", limit))
	if ggufOnly {
		params.Set("filter", "gguf")
	}
	u := fmt.Sprintf("%s/api/models?%s",
		strings.TrimRight(c.Endpoint, "/"), params.Encode())

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.applyAuth(req)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hf search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hf search: %s", resp.Status)
	}
	var out []SearchHit
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode hf search: %w", err)
	}
	return out, nil
}
