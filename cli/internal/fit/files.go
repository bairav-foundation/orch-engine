// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package fit

import (
	"regexp"
	"sort"
	"strings"
)

// File is one GGUF in a repo, sized for a device.
type File struct {
	Path    string // what `bairav model deploy --file` takes
	Quant   string // "Q4_K_M", "Q8_0", ... ("" if the name doesn't say)
	Size    int64  // exact, from the repo
	Need    int64
	Verdict Verdict
}

var splitRe = regexp.MustCompile(`(?i)-\d{5}-of-\d{5}\.gguf$`)

// Files sizes every single-file GGUF in a repo for a device with have
// bytes, largest first. Split models (-00001-of-00003.gguf) and vision
// projectors (mmproj) are left out: bairav serves one GGUF file.
func Files(paths []string, sizes []int64, params, have int64, ctx int) []File {
	var out []File
	for i, p := range paths {
		low := strings.ToLower(p)
		if !strings.HasSuffix(low, ".gguf") || splitRe.MatchString(p) || strings.Contains(low, "mmproj") {
			continue
		}
		f := File{Path: p, Quant: QuantOf(p), Size: sizes[i]}
		f.Need = Need(f.Size, params, ctx)
		f.Verdict = Judge(f.Need, have)
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out
}

// Best is the file to recommend: the largest that fits with room to
// spare, but no bigger than Q8_0 when a Q8_0 or smaller fits (F16/BF16 and
// F32 cost twice the memory and run slower for no visible gain). nil when
// nothing fits.
func Best(files []File) *File {
	var fallback *File
	for i := range files {
		f := &files[i]
		if f.Verdict != Fits {
			continue
		}
		switch f.Quant {
		case "F16", "BF16", "F32":
			if fallback == nil {
				fallback = f
			}
			continue
		}
		return f // largest non-float file that fits
	}
	return fallback
}
