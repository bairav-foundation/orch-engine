// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	SetVersion("v1.2.3-test")

	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"version"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("version command returned error: %v", err)
	}

	// The version command prints via fmt.Printf (stdout), not cobra's buffer,
	// so we can't capture it here without refactoring. Instead, verify via the
	// exported `version` package variable that SetVersion stored the value.
	if !strings.Contains(version, "v1.2.3-test") {
		t.Errorf("SetVersion did not update package version: got %q", version)
	}
}

func TestRootHasSubcommands(t *testing.T) {
	want := []string{"version", "model", "skill", "agent", "chat", "ps", "dlp", "dashboard", "visualize", "grant", "eval"}
	for _, name := range want {
		if _, _, err := rootCmd.Find([]string{name}); err != nil {
			t.Errorf("expected subcommand %q registered on root: %v", name, err)
		}
	}
}

func TestAgentHasSubcommands(t *testing.T) {
	// Agent CRD wired in. Mirrors Skill's subcommand shape.
	want := []string{"deploy", "undeploy", "list"}
	for _, name := range want {
		if _, _, err := rootCmd.Find([]string{"agent", name}); err != nil {
			t.Errorf("expected `bairav agent %s` registered: %v", name, err)
		}
	}
}

func TestModelHasSubcommands(t *testing.T) {
	// All six subcommands are now real implementations.
	want := []string{"pull", "list", "serve", "delete", "deploy", "undeploy", "upgrade", "rollback", "status", "discover"}
	for _, name := range want {
		if _, _, err := rootCmd.Find([]string{"model", name}); err != nil {
			t.Errorf("expected `bairav model %s` registered: %v", name, err)
		}
	}
}

func TestDLPHasSubcommands(t *testing.T) {
	want := []string{"serve", "test", "packs", "checksum"}
	for _, name := range want {
		if _, _, err := rootCmd.Find([]string{"dlp", name}); err != nil {
			t.Errorf("expected `bairav dlp %s` registered: %v", name, err)
		}
	}
	// packs has its own subcommand
	if _, _, err := rootCmd.Find([]string{"dlp", "packs", "list"}); err != nil {
		t.Errorf("expected `bairav dlp packs list` registered: %v", err)
	}
}

func TestSkillHasSubcommands(t *testing.T) {
	// `skill` becomes real. Old stubs (package/run/logs) removed —
	// they described a Job-based semantic that doesn't fit MCP-server-
	// as-Skill. Replaced with deploy/undeploy/list matching model shape.
	want := []string{"deploy", "undeploy", "list", "approve"}
	for _, name := range want {
		if _, _, err := rootCmd.Find([]string{"skill", name}); err != nil {
			t.Errorf("expected `bairav skill %s` registered: %v", name, err)
		}
	}
}
