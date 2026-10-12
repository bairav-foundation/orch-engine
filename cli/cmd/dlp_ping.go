// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// dlpPingCmd is the container healthcheck for the distroless DLP image
// (no curl inside): exit 0 when the URL answers 200.
var dlpPingCmd = &cobra.Command{
	Use:    "ping <url>",
	Short:  "Exit 0 if <url> answers HTTP 200 (container healthcheck)",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(args[0])
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", args[0], resp.Status)
		}
		return nil
	},
}

func init() { dlpCmd.AddCommand(dlpPingCmd) }
