// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"io"
)

// Config tells a Backend what to serve and where.
type Config struct {
	// ModelPath is the absolute filesystem path to the model file.
	// For llama-server this is a .gguf; for vLLM this would be a safetensors directory.
	ModelPath string

	// ModelName is a human-friendly name reported on /v1/models. Defaults to
	// the basename of ModelPath if empty.
	ModelName string

	// Host is the bind address. "127.0.0.1" by default; "0.0.0.0" to expose
	// to other hosts on the network.
	Host string

	// Port is the TCP port. Backends should refuse to start if it's busy.
	Port int

	// ContextSize is the model context window in tokens. 0 = backend default.
	ContextSize int

	// Stdout/Stderr are where the backend's child process should write. Pass
	// os.Stdout/os.Stderr for foreground UX, or io.Discard / a logfile for
	// daemon-style use.
	Stdout io.Writer
	Stderr io.Writer
}

// Backend is one inference engine adapter.
//
// Lifecycle:
//
//	b := llamaserver.New()
//	if err := b.Available(); err != nil { ... handle missing binary ... }
//	if err := b.Start(ctx, cfg); err != nil { ... }
//	defer b.Stop(context.Background())
//	if err := b.WaitReady(ctx); err != nil { ... }
//	// Backend is now serving on http://<cfg.Host>:<cfg.Port>/v1/...
//	<-ctx.Done()
//
// Backends MUST be safe to call Stop on after a failed Start (idempotent).
type Backend interface {
	// Name is a stable identifier ("llama-server", "vllm", "ollama"). Used
	// for logging and the future --backend flag.
	Name() string

	// Available checks that the backend can run on this machine — typically
	// "is the binary on PATH?" Returns a friendly error explaining how to
	// install it if not.
	Available() error

	// Start launches the inference engine. Non-blocking; the engine boots
	// in the background. Use WaitReady to block until it's serving.
	Start(ctx context.Context, cfg Config) error

	// WaitReady polls the engine until it answers a health check, or until
	// ctx is cancelled. Returns nil when ready, or the polling error otherwise.
	WaitReady(ctx context.Context) error

	// Endpoint is the OpenAI-compatible base URL — typically
	// "http://<host>:<port>/v1". Valid after a successful Start.
	Endpoint() string

	// PID returns the spawned engine's process ID, or 0 if not started.
	// Used to populate run-state files and validate liveness in `bairav ps`.
	PID() int

	// Stop terminates the engine. Idempotent. Should send SIGTERM, wait
	// briefly, then SIGKILL if the process hasn't exited.
	Stop(ctx context.Context) error
}

// ErrNotAvailable is returned by Backend.Available when the underlying engine
// binary is not installed or not on PATH. Wrap with %w when returning so callers
// can errors.Is-check it.
var ErrNotAvailable = errors.New("backend not available on this machine")
