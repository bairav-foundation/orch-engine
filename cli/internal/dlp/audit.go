// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileAuditWriter writes one JSONL record per audit entry to the given
// file. Safe for concurrent use by multiple goroutines — the proxy can be
// hit in parallel by many clients.
type FileAuditWriter struct {
	mu sync.Mutex
	w  io.Writer
	// f is non-nil iff we own the file handle (constructed via OpenFile).
	// Close() closes it; an external io.Writer is the caller's problem.
	f *os.File
}

// NewFileAuditWriter opens (or creates) path for append. Creates the
// parent directory if needed. The returned writer must be Close()d at
// shutdown to flush.
func NewFileAuditWriter(path string) (*FileAuditWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("dlp audit: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // audit records PII metadata
	if err != nil {
		return nil, fmt.Errorf("dlp audit: open: %w", err)
	}
	return &FileAuditWriter{w: f, f: f}, nil
}

// NewWriterAuditWriter wraps an arbitrary io.Writer. Used in tests.
func NewWriterAuditWriter(w io.Writer) *FileAuditWriter {
	return &FileAuditWriter{w: w}
}

// Write serialises entry as one JSON line, terminated by "\n".
func (a *FileAuditWriter) Write(entry AuditEntry) error {
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err = a.w.Write(body)
	return err
}

// Close releases the underlying file handle if we own it. Safe to call
// multiple times.
func (a *FileAuditWriter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		err := a.f.Close()
		a.f = nil
		return err
	}
	return nil
}
