// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// PolicyVersion is the currently-supported schema version. Bumping this
// number is a breaking-change signal for policy files in the wild.
const PolicyVersion = "v1"

// Policy is the parsed representation of a user-supplied DLP policy file.
// Right now the only thing a policy declares is additional detector rules
// stacked on top of the built-ins; future versions may add global knobs
// (per-tenant defaults, allow-lists, redact placeholder templates).
type Policy struct {
	Version string `yaml:"version"`
	Rules   []Rule `yaml:"rules"`
}

// Rule is one user-defined detection rule.
type Rule struct {
	// Label is the canonical name used in [REDACTED:LABEL] placeholders and
	// audit records. Must be uppercase letters/digits/underscore, non-empty,
	// and not collide with a built-in label (EMAIL / SSN / CREDIT_CARD /
	// PHONE_US / AWS_ACCESS_KEY).
	Label string `yaml:"label"`

	// Pattern is a Go RE2 regular expression. The regex is compiled at load
	// time; a bad pattern surfaces as a load error, not a runtime panic.
	Pattern string `yaml:"pattern"`

	// Action is an optional per-rule enforcement override. One of "redact",
	// "block", or "audit_only". Empty means "use whatever the proxy is
	// configured with globally via --action".
	Action Action `yaml:"action,omitempty"`

	// Checksum is an OPTIONAL digit-checksum validator applied to every
	// regex match. When set, the rule only reports a hit when the match
	// (with non-digits stripped) passes the named algorithm. Supported
	// values: "luhn" (credit-card checksum, same as CREDIT_CARD built-in),
	// "mod10" (alias for luhn), "verhoeff" (Aadhaar and similar).
	//
	// This is what stops a pure-regex Aadhaar rule from false-positiving
	// on every random 12-digit sequence. Empty means "no checksum, take
	// every regex match as-is."
	Checksum string `yaml:"checksum,omitempty"`

	// Context lists keywords (case-insensitive) that must appear within
	// ContextWindow characters before or after a match for it to count.
	// A one-digit checksum still lets ~10% of random numbers through, so
	// weak-checksum IDs (Aadhaar, NHS) need a nearby "aadhaar" / "nhs" to
	// be reported with confidence. Empty = no context requirement.
	Context []string `yaml:"context,omitempty"`

	// ContextWindow is the distance in characters (default 48).
	ContextWindow int `yaml:"context_window,omitempty"`

	// ContextScope narrows when Context is required: "all" (default) or
	// "ungrouped" (only matches with no separators need a keyword; a
	// formatted "2345 6789 0124" counts on its own).
	ContextScope string `yaml:"context_scope,omitempty"`
}

// labelRe is the accepted shape for rule labels. We deliberately require
// uppercase to match the built-ins visually (EMAIL, SSN, PAN, …) so audit
// log readers can eyeball "custom vs built-in" cheaply.
var labelRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// builtinLabels is the reserved set. Loading a policy whose label collides
// with one of these fails at load time — otherwise a user might think their
// custom rule was firing when in fact the built-in was.
//
// "PAN" was previously reserved (meaning credit card / Primary Account
// Number). It's now free — the credit-card detector is CREDIT_CARD, and
// the India preset pack claims PAN for the Indian tax ID (Permanent
// Account Number). See examples/policies/india.yaml.
var builtinLabels = map[string]bool{
	"EMAIL":          true,
	"SSN":            true,
	"CREDIT_CARD":    true,
	"PHONE_US":       true,
	"AWS_ACCESS_KEY": true,
}

func init() {
	for _, l := range secretLabels() {
		builtinLabels[l] = true
	}
}

// LoadPolicyFile reads and parses a policy YAML file. Returns a compiled
// []Detector ready to be handed to Scan alongside DefaultDetectors().
// Errors carry the file path so misconfigurations are traceable.
func LoadPolicyFile(path string) ([]Detector, *Policy, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("dlp policy: read %s: %w", path, err)
	}
	return ParsePolicy(body, path)
}

// LoadPolicyFiles composes multiple policy files into one detector set
// and one merged Policy. Rules from every file stack on top of the
// built-ins; a label defined in two files fails at load with a clear
// pointer at both files, so a user assembling packs (india.yaml +
// corp.yaml + ...) hears about ambiguities immediately rather than
// silently having one file shadow the other.
//
// Returns nil, nil, nil when paths is empty — the caller then falls
// back to DefaultDetectors() with no custom rules.
func LoadPolicyFiles(paths []string) ([]Detector, *Policy, error) {
	if len(paths) == 0 {
		return nil, nil, nil
	}
	// Track which file first defined each label so a collision message
	// can say WHERE the conflict is, not just that it exists.
	labelSource := map[string]string{}
	merged := &Policy{Version: PolicyVersion}
	var allDetectors []Detector
	for _, path := range paths {
		dets, p, err := LoadPolicyFile(path)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range p.Rules {
			if other, dup := labelSource[r.Label]; dup {
				return nil, nil, fmt.Errorf(
					"dlp policy: label %q is defined in both %s and %s — remove one or rename",
					r.Label, other, path)
			}
			labelSource[r.Label] = path
			merged.Rules = append(merged.Rules, r)
		}
		allDetectors = append(allDetectors, dets...)
	}
	return allDetectors, merged, nil
}

// ParsePolicy parses body as a Policy. path is used only for error messages;
// pass "" if reading from an anonymous source (tests, stdin, embedded).
func ParsePolicy(body []byte, path string) ([]Detector, *Policy, error) {
	var p Policy
	if err := yaml.Unmarshal(body, &p); err != nil {
		return nil, nil, fmt.Errorf("dlp policy %s: parse yaml: %w", pathOrAnon(path), err)
	}
	if err := validatePolicy(&p, path); err != nil {
		return nil, nil, err
	}
	dets := make([]Detector, 0, len(p.Rules))
	for _, r := range p.Rules {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			// Shouldn't happen — validatePolicy already checked — but be
			// defensive so a schema change never turns compile errors
			// into panics.
			return nil, nil, fmt.Errorf("dlp policy %s: rule %q: compile pattern: %w",
				pathOrAnon(path), r.Label, err)
		}
		// Resolve the checksum function from the registry (validated
		// above so the lookup is guaranteed to succeed when non-empty).
		var checksumFn ChecksumFn
		if r.Checksum != "" {
			checksumFn = checksumByName[strings.ToLower(r.Checksum)]
		}
		window := r.ContextWindow
		if window <= 0 {
			window = 48
		}
		var kws []string
		for _, k := range r.Context {
			kws = append(kws, strings.ToLower(k))
		}
		dets = append(dets, regexRuleDetector{
			label:         r.Label,
			re:            re,
			action:        r.Action,
			checksum:      checksumFn,
			context:       kws,
			window:        window,
			ungroupedOnly: r.ContextScope == "ungrouped",
		})
	}
	return dets, &p, nil
}

// validatePolicy performs every check that doesn't require actually running
// the regex. All errors surface the offending rule label / path so the
// operator can grep the source file directly.
func validatePolicy(p *Policy, path string) error {
	if p.Version == "" {
		return fmt.Errorf("dlp policy %s: missing `version` (expected %q)", pathOrAnon(path), PolicyVersion)
	}
	if p.Version != PolicyVersion {
		return fmt.Errorf("dlp policy %s: unsupported version %q (this CLI understands %q)",
			pathOrAnon(path), p.Version, PolicyVersion)
	}
	if len(p.Rules) == 0 {
		return fmt.Errorf("dlp policy %s: `rules` is empty — a policy with no rules is a no-op", pathOrAnon(path))
	}
	seenLabels := map[string]bool{}
	for i, r := range p.Rules {
		if r.Label == "" {
			return fmt.Errorf("dlp policy %s: rule[%d] has empty `label`", pathOrAnon(path), i)
		}
		if !labelRe.MatchString(r.Label) {
			return fmt.Errorf("dlp policy %s: rule[%d] label %q must be uppercase letters/digits/underscore (e.g. \"CUST_ID\")",
				pathOrAnon(path), i, r.Label)
		}
		if builtinLabels[r.Label] {
			return fmt.Errorf("dlp policy %s: rule[%d] label %q collides with a built-in detector (pick a different name)",
				pathOrAnon(path), i, r.Label)
		}
		if seenLabels[r.Label] {
			return fmt.Errorf("dlp policy %s: rule[%d] label %q is defined more than once",
				pathOrAnon(path), i, r.Label)
		}
		seenLabels[r.Label] = true
		if r.Pattern == "" {
			return fmt.Errorf("dlp policy %s: rule %q has empty `pattern`", pathOrAnon(path), r.Label)
		}
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return fmt.Errorf("dlp policy %s: rule %q: invalid regex: %w", pathOrAnon(path), r.Label, err)
		}
		if r.Action != "" {
			if !validAction(r.Action) {
				return fmt.Errorf("dlp policy %s: rule %q: unknown action %q (use redact | block | audit_only)",
					pathOrAnon(path), r.Label, r.Action)
			}
		}
		switch r.ContextScope {
		case "", "all", "ungrouped":
		default:
			return fmt.Errorf("dlp policy %s: rule %q: unknown context_scope %q (use all | ungrouped)",
				pathOrAnon(path), r.Label, r.ContextScope)
		}
		if r.ContextScope != "" && len(r.Context) == 0 {
			return fmt.Errorf("dlp policy %s: rule %q: context_scope needs a context keyword list", pathOrAnon(path), r.Label)
		}
		if r.Checksum != "" {
			if _, ok := checksumByName[strings.ToLower(r.Checksum)]; !ok {
				return fmt.Errorf("dlp policy %s: rule %q: unknown checksum %q (available: luhn, mod10, verhoeff)",
					pathOrAnon(path), r.Label, r.Checksum)
			}
		}
	}
	return nil
}

// validAction reports whether a is one of the three known Action values.
func validAction(a Action) bool {
	switch a {
	case ActionRedact, ActionBlock, ActionAuditOnly:
		return true
	}
	return false
}

func pathOrAnon(p string) string {
	if p == "" {
		return "<anonymous>"
	}
	return p
}

// regexRuleDetector is a Detector backed by a user-supplied regex, an
// optional per-rule action, and an optional digit-checksum filter. It's
// the runtime representation of a Rule after parsing.
type regexRuleDetector struct {
	label    string
	re       *regexp.Regexp
	action   Action
	checksum ChecksumFn // nil means "no checksum, accept every regex match"
	// context: lower-cased keywords required near a match (Rule.Context).
	context       []string
	window        int
	ungroupedOnly bool
}

// hasContext reports whether a keyword appears within window characters of
// text[start:end]. Window bounds are snapped to rune boundaries.
func (r regexRuleDetector) hasContext(text string, start, end int) bool {
	lo, hi := start-r.window, end+r.window
	if lo < 0 {
		lo = 0
	}
	if hi > len(text) {
		hi = len(text)
	}
	for lo > 0 && !utf8.RuneStart(text[lo]) {
		lo--
	}
	for hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi++
	}
	around := strings.ToLower(text[lo:start] + " " + text[end:hi])
	for _, k := range r.context {
		if strings.Contains(around, k) {
			return true
		}
	}
	return false
}

func (r regexRuleDetector) Name() string { return r.label }

func (r regexRuleDetector) Detect(text string) []Hit {
	idxs := r.re.FindAllStringIndex(text, -1)
	if len(idxs) == 0 {
		return nil
	}
	hits := make([]Hit, 0, len(idxs))
	for _, idx := range idxs {
		match := text[idx[0]:idx[1]]
		if r.checksum != nil {
			// A rule with `checksum:` set only reports matches whose
			// digit-only form passes the algorithm. Without this, an
			// Aadhaar-shaped regex would false-positive on every
			// random 12-digit number.
			digits := stripNonDigits(match)
			if !r.checksum(digits) {
				continue
			}
		}
		if len(r.context) > 0 {
			grouped := len(stripNonDigits(match)) != len(match)
			if (!r.ungroupedOnly || !grouped) && !r.hasContext(text, idx[0], idx[1]) {
				continue
			}
		}
		hits = append(hits, Hit{
			Label:  r.label,
			Start:  idx[0],
			End:    idx[1],
			Match:  match,
			Action: r.action, // may be "" — proxy falls back to global
		})
	}
	return hits
}

// SortedRuleLabels returns the labels of a Policy in sorted order — handy
// for deterministic printing in dlp serve's startup banner.
func SortedRuleLabels(p *Policy) []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Rules))
	for _, r := range p.Rules {
		out = append(out, r.Label)
	}
	sort.Strings(out)
	return out
}
