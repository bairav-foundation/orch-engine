// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"math"
)

// portFlag checks a --port value and converts it to the int32 the CRDs use.
func portFlag(v int) (int32, error) {
	if v < 1 || v > 65535 {
		return 0, fmt.Errorf("--port %d: must be 1-65535", v)
	}
	return int32(v), nil
}

// ctxSizeFlag checks a --ctx-size value (0 = the model's default).
func ctxSizeFlag(v int) (int32, error) {
	if v < 0 || v > math.MaxInt32 {
		return 0, fmt.Errorf("--ctx-size %d: must be between 0 and %d", v, math.MaxInt32)
	}
	return int32(v), nil
}
