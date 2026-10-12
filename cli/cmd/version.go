// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the bairav CLI version",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("bairav %s\n", version)
		return nil
	},
}
