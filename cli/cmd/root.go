// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package cmd holds the Cobra commands for the bairav CLI.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/bairav-foundation/orch-engine/cli/internal/compose"
)

var version = "v0.0.0-dev"

// SetVersion is called from main before Execute to thread the build-time version
// into the CLI without the cmd package depending on main.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
	rootCmd.Version = version
	// Release binaries pull the matching published images; dev builds use
	// the locally built :laptop tags.
	compose.UseReleaseImages(version)
}

var rootCmd = &cobra.Command{
	Use:   "bairav",
	Short: "Run models, MCP tools and agents on Kubernetes or your laptop, with DLP built in",
	Long: `BAIRAVS Orch Engine runs models, MCP tools (skills) and agents as one
connected graph, on Docker or Kubernetes, with data-loss protection built in.

  bairav model deploy <hf-repo>        serve an open-weight model (or --provider openai|anthropic)
  bairav skill deploy <image>          run an MCP server
  bairav agent deploy <image> --model m --skill s
  bairav ps | dashboard                see the graph
  bairav dlp test "text"               try the DLP detectors

Docs: https://tech.bairavs.com/orch-engine/docs/`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the CLI. Any command that returns an error will have that error
// printed here, so callers (main) only need to check the return for the exit code.
func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "bairav: %v\n", err)
		return err
	}
	return nil
}

func init() {
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(modelCmd)
	rootCmd.AddCommand(skillCmd)
	rootCmd.AddCommand(agentCmd)
	rootCmd.AddCommand(chatCmd)
	rootCmd.AddCommand(psCmd)
	rootCmd.AddCommand(dlpCmd)
}
