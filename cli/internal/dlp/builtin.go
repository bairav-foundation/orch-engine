// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"regexp"
)

// DefaultDetectors returns the v1 built-in detector set. Order does not
// matter; Scan dedupes overlapping hits and prefers longer matches.
func DefaultDetectors() []Detector {
	return []Detector{
		emailDetector{},
		ssnDetector{},
		creditCardDetector{},
		usPhoneDetector{},
		awsAccessKeyDetector{},
		// Credentials (secrets.go): prefix-anchored formats, so precise
		// enough to be on for everyone. The source-code / key leak is the
		// incident that made "AI DLP" a category.
		secretsDetector{},
	}
}

// -----------------------------------------------------------------------------
// EMAIL
// -----------------------------------------------------------------------------

// Loose-but-not-too-loose: alphanum/dot/underscore/hyphen/plus left of '@',
// alphanum/dot/hyphen right of '@', minimum 2-letter TLD. We do not try to be
// RFC-5322-correct; the cost of false negatives on weird-but-valid addresses
// is lower than the cost of false positives on prose punctuation.
var emailRe = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)

type emailDetector struct{}

func (emailDetector) Name() string { return "EMAIL" }
func (emailDetector) Detect(text string) []Hit {
	return runRegexp(text, emailRe, "EMAIL")
}

// -----------------------------------------------------------------------------
// US SSN
// -----------------------------------------------------------------------------

// Grouped forms (NNN-NN-NNNN, NNN NN NNNN, NNN.NN.NNNN) are matched on
// shape. A bare 9-digit run is far too common (order numbers, ZIP+4) and
// only counts with an SSN keyword just before it. Numbers the SSA never
// issues (area 000/666/9xx, group 00, serial 0000) are rejected.
var (
	ssnGroupedRe = regexp.MustCompile(`\b\d{3}([-. ])\d{2}([-. ])\d{4}\b`)
	ssnBareRe    = regexp.MustCompile(`(?i)\b(?:ssn|social security(?: number| no\.?| #)?)\W{0,4}(\d{9})\b`)
)

type ssnDetector struct{}

func (ssnDetector) Name() string { return "SSN" }
func (ssnDetector) Detect(text string) []Hit {
	var hits []Hit
	for _, m := range ssnGroupedRe.FindAllStringSubmatchIndex(text, -1) {
		// Same separator twice: "123-45 6789" is not an SSN layout.
		if text[m[2]:m[3]] != text[m[4]:m[5]] {
			continue
		}
		if ssnIssuable(stripNonDigits(text[m[0]:m[1]])) {
			hits = append(hits, Hit{Label: "SSN", Start: m[0], End: m[1], Match: text[m[0]:m[1]]})
		}
	}
	for _, m := range ssnBareRe.FindAllStringSubmatchIndex(text, -1) {
		if ssnIssuable(text[m[2]:m[3]]) {
			hits = append(hits, Hit{Label: "SSN", Start: m[2], End: m[3], Match: text[m[2]:m[3]]})
		}
	}
	return hits
}

// ssnIssuable applies the SSA's structural rules to 9 digits.
func ssnIssuable(d string) bool {
	if len(d) != 9 {
		return false
	}
	area, group, serial := d[:3], d[3:5], d[5:]
	return area != "000" && area != "666" && area[0] != '9' && group != "00" && serial != "0000"
}

// -----------------------------------------------------------------------------
// CREDIT_CARD
// -----------------------------------------------------------------------------
//
// Historical note: this detector was originally labelled "PAN" (Primary
// Account Number, the ISO 7812 term for card numbers). Renamed to
// CREDIT_CARD because "PAN" also means Permanent Account Number in India
// (the 10-char tax ID), and the collision would confuse anyone loading
// the India preset pack. See examples/policies/india.yaml where the
// "PAN" label is now claimed for the Indian tax ID.

// Captures digit runs with optional single space/hyphen/dot separators
// (up to 23 digits so a card followed by a CVV or expiry is still one
// candidate), then looks inside each candidate for a 13-19 digit window
// that passes Luhn. Windows must start and end on group boundaries, so a
// card is found in "4111111111111111 123" but not carved out of the middle
// of an unrelated long number.
var creditCardRe = regexp.MustCompile(`\b(?:\d[ .-]?){12,22}\d\b`)

type creditCardDetector struct{}

func (creditCardDetector) Name() string { return "CREDIT_CARD" }
func (creditCardDetector) Detect(text string) []Hit {
	var hits []Hit
	for _, idx := range creditCardRe.FindAllStringIndex(text, -1) {
		if s, e, ok := luhnWindow(text[idx[0]:idx[1]]); ok {
			hits = append(hits, Hit{
				Label: "CREDIT_CARD",
				Start: idx[0] + s,
				End:   idx[0] + e,
				Match: text[idx[0]+s : idx[0]+e],
			})
		}
	}
	return hits
}

// luhnWindow returns the byte span of the longest Luhn-valid 13-19 digit
// window in candidate whose ends fall on group boundaries.
func luhnWindow(candidate string) (start, end int, ok bool) {
	type pos struct {
		at              int
		groupStart, end bool // first digit of a group / last digit of a group
	}
	var ds []pos
	for i := 0; i < len(candidate); i++ {
		if c := candidate[i]; c >= '0' && c <= '9' {
			ds = append(ds, pos{
				at:         i,
				groupStart: i == 0 || !isDigit(candidate[i-1]),
				end:        i == len(candidate)-1 || !isDigit(candidate[i+1]),
			})
		}
	}
	best := 0
	for i := range ds {
		if !ds[i].groupStart {
			continue
		}
		for n := 19; n >= 13; n-- {
			j := i + n - 1
			if j >= len(ds) || !ds[j].end || n <= best {
				continue
			}
			var b []byte
			for k := i; k <= j; k++ {
				b = append(b, candidate[ds[k].at])
			}
			if luhnValid(string(b)) {
				start, end, ok, best = ds[i].at, ds[j].at+1, true, n
			}
		}
	}
	return start, end, ok
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func stripNonDigits(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return string(out)
}

// luhnValid reports whether digits passes the Luhn checksum. digits must
// contain only ASCII '0'-'9'; the caller has already stripped separators.
// Reference: ISO/IEC 7812-1, used by every major card scheme + many IDs.
func luhnValid(digits string) bool {
	if len(digits) == 0 {
		return false
	}
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}

// -----------------------------------------------------------------------------
// US PHONE
// -----------------------------------------------------------------------------

// Captures (XXX) XXX-XXXX, XXX-XXX-XXXX, XXX.XXX.XXXX, XXX XXX XXXX, and the
// leading "+1 " variant. International formats are intentionally out of scope
// for v1 — the false-positive risk on "+92" prose would be high.
var usPhoneRe = regexp.MustCompile(`(?:\+1[\s\-.]?)?\(?\b\d{3}\)?[\s\-.]?\d{3}[\s\-.]?\d{4}\b`)

type usPhoneDetector struct{}

func (usPhoneDetector) Name() string { return "PHONE_US" }
func (usPhoneDetector) Detect(text string) []Hit {
	return runRegexp(text, usPhoneRe, "PHONE_US")
}

// -----------------------------------------------------------------------------
// AWS Access Key ID
// -----------------------------------------------------------------------------

// AWS access key IDs match a fixed prefix + 16 base32-ish chars. There are
// other AWS credential shapes (secrets, session tokens), but the access key
// ID is the high-confidence anchor — if we see one, the secret is almost
// certainly nearby in the same blob.
var awsAccessKeyRe = regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASCA)[0-9A-Z]{16}\b`)

type awsAccessKeyDetector struct{}

func (awsAccessKeyDetector) Name() string { return "AWS_ACCESS_KEY" }
func (awsAccessKeyDetector) Detect(text string) []Hit {
	return runRegexp(text, awsAccessKeyRe, "AWS_ACCESS_KEY")
}

// -----------------------------------------------------------------------------
// shared
// -----------------------------------------------------------------------------

func runRegexp(text string, re *regexp.Regexp, label string) []Hit {
	idxs := re.FindAllStringIndex(text, -1)
	if len(idxs) == 0 {
		return nil
	}
	hits := make([]Hit, 0, len(idxs))
	for _, idx := range idxs {
		hits = append(hits, Hit{
			Label: label,
			Start: idx[0],
			End:   idx[1],
			Match: text[idx[0]:idx[1]],
		})
	}
	return hits
}
