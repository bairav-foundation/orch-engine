// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// State describes one running backend, persisted to disk so other CLI
// invocations (`bairav ps`, `bairav chat`, future `bairav status`) can
// discover what's currently serving without OS-level process scanning.
type State struct {
	Port      int       `json:"port"`
	Host      string    `json:"host"`
	Model     string    `json:"model"`      // user-facing HF repo slug
	ModelFile string    `json:"model_file"` // absolute path on disk
	Backend   string    `json:"backend"`    // e.g. "llama-server"
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// Endpoint reconstructs the OpenAI-compatible URL from host + port.
func (s State) Endpoint() string {
	return fmt.Sprintf("http://%s:%d/v1", s.Host, s.Port)
}

// stateDir is ~/.bairav/run/. $BAIRAV_RUN_DIR overrides for tests.
func stateDir() (string, error) {
	if v := os.Getenv("BAIRAV_RUN_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".bairav", "run"), nil
}

// statePath returns the absolute path of the state file for a given port.
func statePath(port int) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("port-%d.json", port)), nil
}

// WriteState writes (or replaces) a state file atomically: write a sibling
// .tmp, fsync, rename.
func WriteState(s State) error {
	path, err := statePath(s.Port)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir run: %w", err)
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("write tmp state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// RemoveState deletes the state file for a port. Idempotent — missing file
// is not an error, since clean shutdown can race with `ps`-driven pruning.
func RemoveState(port int) error {
	path, err := statePath(port)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ReadAllStates returns every well-formed state file in stateDir, sorted by
// port. Missing dir or unreadable individual files are silently skipped —
// this is a best-effort discovery API, not a critical path.
func ReadAllStates() ([]State, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("read run dir: %w", err)
	}
	var out []State
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "port-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var s State
		if err := json.Unmarshal(body, &s); err != nil {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

// PIDAlive returns true if the given PID is a running process. On Unix this
// uses signal 0 (no-op signal that returns an error if the process is gone).
// Returns false for PID <= 0 or if FindProcess/Signal errors out.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
