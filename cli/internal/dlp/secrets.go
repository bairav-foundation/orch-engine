// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

// Credential detection. Every rule is anchored on a vendor prefix or a
// fixed structure (gitleaks-style), so a match is almost never anything
// but a real credential. Generic "high-entropy string" heuristics are
// deliberately left out: they fire on hashes, UUIDs and base64 blobs.

import (
	"regexp"
	"strings"
)

type secretRule struct {
	label string
	hint  string // literal that must occur for the regex to have a chance (cheap pre-filter)
	re    *regexp.Regexp
	group int // submatch holding the secret; 0 = whole match
}

var secretRules = []secretRule{
	{"ANTHROPIC_KEY", "sk-ant-", regexp.MustCompile(`\bsk-ant-(?:api|admin)\d{2}-[A-Za-z0-9_\-]{80,120}`), 0},
	{"OPENAI_KEY", "sk-", regexp.MustCompile(`\bsk-(?:proj|svcacct|admin)-[A-Za-z0-9_\-]{20,200}`), 0},
	{"OPENAI_KEY", "sk-", regexp.MustCompile(`\bsk-[A-Za-z0-9]{20}T3BlbkFJ[A-Za-z0-9]{20}\b`), 0}, // legacy format
	{"GITHUB_TOKEN", "_", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b`), 0},
	{"GITHUB_TOKEN", "github_pat_", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{82}\b`), 0},
	{"GITLAB_TOKEN", "glpat-", regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20}\b`), 0},
	{"SLACK_TOKEN", "xox", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,72}`), 0},
	{"STRIPE_KEY", "_live_", regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{20,99}\b`), 0},
	{"GOOGLE_API_KEY", "AIza", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), 0},
	{"HF_TOKEN", "hf_", regexp.MustCompile(`\bhf_[A-Za-z0-9]{34,40}\b`), 0},
	{"AZURE_STORAGE_KEY", "AccountKey=", regexp.MustCompile(`AccountKey=([A-Za-z0-9+/]{86}==)`), 1},
	{"AWS_SECRET_KEY", "", regexp.MustCompile(`(?i)aws.{0,20}secret.{0,20}?['"=:\s]{1,5}([A-Za-z0-9/+]{40})\b`), 1},
	{"JWT", "eyJ", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), 0},
	{"PRIVATE_KEY", "PRIVATE KEY", regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----(?:[A-Za-z0-9+/=\s:,.\-]*?-----END (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----)?`), 0},
}

// secretLabels are the labels secretsDetector can emit (reserved: custom
// policy rules can't reuse them).
func secretLabels() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range secretRules {
		if !seen[r.label] {
			seen[r.label] = true
			out = append(out, r.label)
		}
	}
	return out
}

type secretsDetector struct{}

func (secretsDetector) Name() string { return "SECRETS" }
func (secretsDetector) Detect(text string) []Hit {
	var hits []Hit
	for _, r := range secretRules {
		if r.hint != "" && !strings.Contains(text, r.hint) {
			continue
		}
		if r.label == "AWS_SECRET_KEY" && !containsFold(text, "aws") {
			continue
		}
		for _, m := range r.re.FindAllStringSubmatchIndex(text, -1) {
			s, e := m[2*r.group], m[2*r.group+1]
			if s < 0 {
				continue
			}
			hits = append(hits, Hit{Label: r.label, Start: s, End: e, Match: text[s:e]})
		}
	}
	return hits
}

// containsFold is an allocation-free ASCII case-insensitive Contains.
func containsFold(s, sub string) bool {
	n := len(sub)
	for i := 0; i+n <= len(s); i++ {
		if strings.EqualFold(s[i:i+n], sub) {
			return true
		}
	}
	return false
}
