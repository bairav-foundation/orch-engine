// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package fit

import (
	"regexp"
	"sort"
	"strings"
)

// File is one GGUF model in a repo, sized for a device.
type File struct {
	Path    string // what `bairav model deploy --file` takes (first part, for split models)
	Quant   string // "Q4_K_M", "Q8_0", ... ("" if the name doesn't say)
	Size    int64  // exact, from the repo (all parts together for split models)
	Parts   int    // >1 for a model split into several files (-00001-of-00003.gguf)
	Legacy  bool   // an old format current llama.cpp no longer loads
	Need    int64
	Verdict Verdict
}

// Servable reports whether bairav can serve this file today: one file,
// in a format current llama.cpp loads.
func (f File) Servable() bool { return f.Parts <= 1 && !f.Legacy }

var (
	splitRe = regexp.MustCompile(`(?i)-(\d{5})-of-(\d{5})\.gguf$`)
	// Q4_0_4_4, Q4_0_4_8, Q4_0_8_8: ARM repacks that llama.cpp dropped in
	// favour of repacking Q4_0 at load time.
	legacyRe = regexp.MustCompile(`^Q4_0_\d_\d$`)
)

// Files sizes every GGUF model in a repo for a device with have bytes,
// largest first. A model split into parts is one entry with the parts'
// total size. Vision projectors (mmproj) are left out.
func Files(paths []string, sizes []int64, params, have int64, ctx int) []File {
	var out []File
	split := map[string]int{} // base name -> index in out
	for i, p := range paths {
		var size int64
		if i < len(sizes) {
			size = sizes[i]
		}
		low := strings.ToLower(p)
		if !strings.HasSuffix(low, ".gguf") || strings.Contains(low, "mmproj") {
			continue
		}
		if m := splitRe.FindStringSubmatch(p); m != nil {
			base := p[:len(p)-len(m[0])]
			if j, ok := split[base]; ok {
				out[j].Size += size
				if m[1] == "00001" {
					out[j].Path = p
				}
				continue
			}
			split[base] = len(out)
			parts := 0
			for _, c := range m[2] {
				parts = parts*10 + int(c-'0')
			}
			out = append(out, File{Path: p, Quant: QuantOf(base + ".gguf"), Size: size, Parts: parts})
			continue
		}
		q := QuantOf(p)
		out = append(out, File{Path: p, Quant: q, Size: size, Legacy: legacyRe.MatchString(q)})
	}
	for i := range out {
		out[i].Need = Need(out[i].Size, params, ctx)
		out[i].Verdict = Judge(out[i].Need, have)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out
}

// Best is the file to recommend: the largest servable file that fits with
// room to spare, but no float (F16/BF16/F32) when a quantised one fits:
// those cost twice the memory and run slower for no visible gain. nil when
// nothing servable fits.
func Best(files []File) *File {
	return best(files, true)
}

// BestAny is Best including files bairav can't serve yet (split models):
// what would be ideal on this machine.
func BestAny(files []File) *File {
	return best(files, false)
}

func best(files []File, servableOnly bool) *File {
	var fallback *File
	for i := range files {
		f := &files[i]
		if f.Verdict != Fits || f.Legacy || (servableOnly && !f.Servable()) {
			continue
		}
		switch f.Quant {
		case "F16", "BF16", "F32":
			if fallback == nil {
				fallback = f
			}
			continue
		}
		return f // largest quantised file that fits
	}
	return fallback
}

// LowQuality reports whether a quant loses noticeable quality (below 4-bit).
func LowQuality(q string) bool {
	return strings.HasPrefix(q, "Q2") || strings.HasPrefix(q, "Q3") ||
		strings.HasPrefix(q, "IQ1") || strings.HasPrefix(q, "IQ2") || strings.HasPrefix(q, "IQ3")
}
