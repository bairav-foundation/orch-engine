// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package llamaserver

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bairav-foundation/orch-engine/cli/internal/serve"
)

// binaryName is the executable we look for on PATH. The upstream llama.cpp
// project renamed `server` → `llama-server` in 2024. Homebrew installs it as
// `llama-server`; some Linux distros keep `server`. We try both.
var candidateBinaries = []string{"llama-server", "server"}

// Backend implements serve.Backend by supervising a llama-server child process.
type Backend struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	endpoint string
}

// New returns a fresh, unstarted Backend.
func New() *Backend { return &Backend{} }

// Name returns the stable backend identifier.
func (b *Backend) Name() string { return "llama-server" }

// Available verifies that a usable llama-server binary is on PATH.
func (b *Backend) Available() error {
	if _, err := locateBinary(); err != nil {
		return fmt.Errorf("%w: %v", serve.ErrNotAvailable, err)
	}
	return nil
}

// Start launches llama-server with the given config. Non-blocking; use
// WaitReady to block until /health returns 200.
func (b *Backend) Start(ctx context.Context, cfg serve.Config) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cmd != nil {
		return fmt.Errorf("backend already started (pid %d)", b.cmd.Process.Pid)
	}

	if cfg.ModelPath == "" {
		return fmt.Errorf("config.ModelPath is required")
	}
	if _, err := os.Stat(cfg.ModelPath); err != nil {
		return fmt.Errorf("model file: %w", err)
	}
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Port
	if port == 0 {
		port = 8080
	}
	modelName := cfg.ModelName
	if modelName == "" {
		modelName = filepath.Base(cfg.ModelPath)
	}

	// Refuse to start if the port is already in use — clearer error than
	// letting llama-server fail with a cryptic bind error.
	if err := checkPortFree(host, port); err != nil {
		return err
	}

	bin, err := locateBinary()
	if err != nil {
		return fmt.Errorf("%w: %v", serve.ErrNotAvailable, err)
	}

	args := []string{
		"-m", cfg.ModelPath,
		"--host", host,
		"--port", strconv.Itoa(port),
		"--alias", modelName,
	}
	if cfg.ContextSize > 0 {
		args = append(args, "-c", strconv.Itoa(cfg.ContextSize))
	}

	// Plain exec.Command — not CommandContext. We don't want ctx cancellation
	// to race the graceful SIGTERM-then-SIGKILL sequence inside Stop().
	// The caller is expected to defer backend.Stop().
	_ = ctx                           // ctx reserved for future use (e.g., per-spawn timeout)
	cmd := exec.Command(bin, args...) //nolint:gosec // bin path is from PATH lookup
	if cfg.Stdout != nil {
		cmd.Stdout = cfg.Stdout
	} else {
		cmd.Stdout = io.Discard
	}
	if cfg.Stderr != nil {
		cmd.Stderr = cfg.Stderr
	} else {
		cmd.Stderr = io.Discard
	}
	// Put the child in its own process group so we can signal the whole tree
	// on Stop (llama-server may spawn worker threads/subprocesses).
	cmd.SysProcAttr = sysProcAttrNewGroup()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", bin, err)
	}
	b.cmd = cmd
	b.endpoint = fmt.Sprintf("http://%s:%d/v1", host, port)
	return nil
}

// WaitReady polls the bound port for /health until it returns 200, or until
// ctx is cancelled. Polls every 200ms. Most models load in 1-15 seconds.
func (b *Backend) WaitReady(ctx context.Context) error {
	b.mu.Lock()
	endpoint := b.endpoint
	b.mu.Unlock()
	if endpoint == "" {
		return fmt.Errorf("backend not started")
	}

	healthURL := endpoint[:len(endpoint)-len("/v1")] + "/health"
	httpClient := &http.Client{Timeout: 1 * time.Second}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("backend not ready: %w", ctx.Err())
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
			if err != nil {
				continue
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			// Some llama.cpp builds return 503 while loading — treat as "not yet".
		}
	}
}

// Endpoint returns the OpenAI-compatible base URL once the backend has started.
func (b *Backend) Endpoint() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.endpoint
}

// PID returns the spawned engine's process ID, or 0 if not started.
func (b *Backend) PID() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil || b.cmd.Process == nil {
		return 0
	}
	return b.cmd.Process.Pid
}

// Stop terminates the backend. Sends SIGTERM, waits up to 5s, then SIGKILL.
// Idempotent: safe to call after a failed Start, or twice in a row.
func (b *Backend) Stop(ctx context.Context) error {
	b.mu.Lock()
	cmd := b.cmd
	b.cmd = nil
	b.endpoint = ""
	b.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	// Signal the whole process group, not just the leader.
	if err := signalGroup(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		// Process may already be gone — fall through to wait.
		_ = err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// After SIGTERM the process exits non-zero by design, so a clean stop
	// ignores Wait's error; the other paths report it.
	select {
	case waitErr := <-done:
		_ = waitErr
		return nil
	case <-time.After(5 * time.Second):
		_ = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		waitErr := <-done
		return fmt.Errorf("backend did not exit on SIGTERM; SIGKILL sent (%v)", waitErr)
	case <-ctx.Done():
		_ = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		waitErr := <-done
		return fmt.Errorf("%w (backend killed: %v)", ctx.Err(), waitErr)
	}
}

// locateBinary looks for any of our candidate binary names on PATH.
func locateBinary() (string, error) {
	for _, name := range candidateBinaries {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("none of %v found on PATH (install with `brew install llama.cpp` on macOS)", candidateBinaries)
}

// checkPortFree returns nil if nothing is listening on host:port. We try a
// brief listen to be sure — net.Dial is unreliable for "is something there".
func checkPortFree(host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("port %d busy: %w (run `lsof -i :%d` to see who)", port, err, port)
	}
	_ = ln.Close()
	return nil
}

// sysProcAttrNewGroup returns a SysProcAttr that puts the child in a new
// process group on Unix. On Windows this is a no-op (different mechanism).
func sysProcAttrNewGroup() *syscall.SysProcAttr {
	if runtime.GOOS == "windows" {
		return nil
	}
	return &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to every process in pid's process group on Unix.
// On Windows, falls back to signalling the single process.
func signalGroup(pid int, sig syscall.Signal) error {
	if runtime.GOOS == "windows" {
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return p.Signal(sig)
	}
	// Negative PID == process group on Unix.
	return syscall.Kill(-pid, sig)
}
