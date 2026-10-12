// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Command bairav is the CLI for BAIRAVS Orch Engine — a K8s-native,
// self-hostable platform for deploying open-source LLMs, packaging AI
// skills/agents, and enforcing DLP on prompts before they leave the perimeter.
package main

import (
	"os"

	"github.com/bairav-foundation/orch-engine/cli/cmd"
)

// Version is injected at build time via -ldflags "-X main.Version=...".
var Version = "v0.0.0-dev"

func main() {
	cmd.SetVersion(Version)
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
