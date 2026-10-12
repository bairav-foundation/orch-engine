// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BAIRAV_RUN_DIR", dir)

	s := State{
		Port:      8080,
		Host:      "127.0.0.1",
		Model:     "Qwen/Qwen2.5-0.5B-Instruct-GGUF",
		ModelFile: "/tmp/model.gguf",
		Backend:   "llama-server",
		PID:       12345,
		StartedAt: time.Now().Truncate(time.Second),
	}

	if err := WriteState(s); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	all, err := ReadAllStates()
	if err != nil {
		t.Fatalf("ReadAllStates: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 state, got %d", len(all))
	}
	got := all[0]
	if got.Port != s.Port || got.Model != s.Model || got.PID != s.PID {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, s)
	}

	// Endpoint() should reconstruct the URL correctly.
	if got.Endpoint() != "http://127.0.0.1:8080/v1" {
		t.Errorf("Endpoint() = %q", got.Endpoint())
	}

	// Remove and confirm.
	if err := RemoveState(s.Port); err != nil {
		t.Fatalf("RemoveState: %v", err)
	}
	all, _ = ReadAllStates()
	if len(all) != 0 {
		t.Errorf("expected empty after RemoveState, got %d entries", len(all))
	}

	// Removing a missing port is a no-op.
	if err := RemoveState(9999); err != nil {
		t.Errorf("RemoveState on missing port should be no-op, got %v", err)
	}
}

func TestReadAllStates_SkipsJunk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BAIRAV_RUN_DIR", dir)

	// One valid + one malformed + one with a non-matching name.
	if err := WriteState(State{Port: 8080, Model: "ok"}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "port-9999.json"), []byte("not-json"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignore"), 0o644)

	all, err := ReadAllStates()
	if err != nil {
		t.Fatalf("ReadAllStates: %v", err)
	}
	if len(all) != 1 || all[0].Port != 8080 {
		t.Errorf("expected only the valid entry, got %+v", all)
	}
}

func TestPIDAlive(t *testing.T) {
	// Self is alive.
	if !PIDAlive(os.Getpid()) {
		t.Error("PIDAlive(self) = false, want true")
	}
	// 0 is never alive.
	if PIDAlive(0) {
		t.Error("PIDAlive(0) = true, want false")
	}
	// Very large PID is highly unlikely to be alive.
	if PIDAlive(999_999_999) {
		t.Error("PIDAlive(huge) = true, want false")
	}
}

func TestReadAllStates_MissingDirReturnsNil(t *testing.T) {
	t.Setenv("BAIRAV_RUN_DIR", filepath.Join(t.TempDir(), "does-not-exist"))
	all, err := ReadAllStates()
	if err != nil {
		t.Errorf("expected nil error for missing dir, got %v", err)
	}
	if all != nil {
		t.Errorf("expected nil slice, got %+v", all)
	}
}
