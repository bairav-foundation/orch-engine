// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// ProgressFn is invoked while a download streams. downloaded is the running
// byte count (cumulative across resumes); total is the file's full size as
// reported by the server, or -1 if the server didn't include Content-Length.
type ProgressFn func(downloaded, total int64)

// Download streams the file at url to dst, with Range-based resume.
//
// Behaviour:
//   - If dst already exists at the full remote size: no-op (return nil).
//   - If dst exists partially: send GET with "Range: bytes=<offset>-" and append.
//   - If the remote server doesn't honour Range (returns 200 instead of 206):
//     the partial is discarded and we re-download from scratch.
//   - Writes go through a .partial sidecar file and are renamed into place at
//     the end so an interrupted download never leaves a half-written file
//     where a complete one was expected.
//
// progress may be nil if the caller doesn't want notifications.
func (c *Client) Download(ctx context.Context, url, dst string, progress ProgressFn) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	partial := dst + ".partial"

	// 1. Probe the remote with HEAD to learn the total size.
	total, err := c.headSize(ctx, url)
	if err != nil {
		return err
	}

	// 2. If the final file already exists at full size, we're done.
	if fi, err := os.Stat(dst); err == nil && total > 0 && fi.Size() == total {
		if progress != nil {
			progress(total, total)
		}
		return nil
	}

	// 3. Look at any existing .partial — figure out where to resume.
	var startOffset int64
	if fi, err := os.Stat(partial); err == nil {
		if total > 0 && fi.Size() > total {
			// partial file is larger than the remote — corrupt; start over.
			_ = os.Remove(partial)
		} else if total > 0 && fi.Size() == total {
			// partial is already complete — promote it and finish.
			if err := os.Rename(partial, dst); err != nil {
				return fmt.Errorf("promote partial: %w", err)
			}
			if progress != nil {
				progress(total, total)
			}
			return nil
		} else {
			startOffset = fi.Size()
		}
	}

	// 4. Issue the GET, with Range header if resuming.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build GET: %w", err)
	}
	c.applyAuth(req)
	if startOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startOffset))
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Server ignored Range or we weren't resuming — start fresh.
		startOffset = 0
		_ = os.Remove(partial)
	case http.StatusPartialContent:
		// Server honoured Range — append to partial.
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: %s — %s", url, resp.Status, string(body))
	}

	// 5. Open the partial file in the right mode.
	flag := os.O_WRONLY | os.O_CREATE
	if startOffset > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(partial, flag, 0o644)
	if err != nil {
		return fmt.Errorf("open partial: %w", err)
	}

	// 6. Stream the body, ticking progress as we go.
	pw := &progressWriter{downloaded: startOffset, total: total, cb: progress}
	if _, err := io.Copy(io.MultiWriter(f, pw), resp.Body); err != nil {
		_ = f.Close()
		return fmt.Errorf("stream %s: %w", url, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync partial: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close partial: %w", err)
	}

	// 7. Atomic rename into the final path.
	if err := os.Rename(partial, dst); err != nil {
		return fmt.Errorf("rename partial → final: %w", err)
	}
	return nil
}

// headSize probes the remote with HEAD and returns Content-Length, or -1
// if the server didn't supply it. A non-200 HEAD is a hard failure.
func (c *Client) headSize(ctx context.Context, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build HEAD: %w", err)
	}
	c.applyAuth(req)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("HEAD %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HEAD %s: %s", url, resp.Status)
	}
	if resp.ContentLength <= 0 {
		return -1, nil
	}
	return resp.ContentLength, nil
}

// progressWriter is an io.Writer that increments a counter and notifies a callback.
type progressWriter struct {
	downloaded int64
	total      int64
	cb         ProgressFn
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.downloaded += int64(n)
	if p.cb != nil {
		p.cb(p.downloaded, p.total)
	}
	return n, nil
}
