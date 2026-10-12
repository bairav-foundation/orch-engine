// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package hf

import "fmt"

// HumanBytes formats a byte count as a short human-readable string:
//
//	          0 → "0 B"
//	      1023 → "1023 B"
//	      1024 → "1.0 KiB"
//	   1572864 → "1.5 MiB"
//	1073741824 → "1.0 GiB"
//
// Uses binary units (1024-based) because that's what disk sizes and model
// quantizations are measured in.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
