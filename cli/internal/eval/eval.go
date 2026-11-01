// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package eval runs a YAML suite of prompts against an OpenAI-compatible
// endpoint and checks each reply: plain text checks (contains, regex, JSON,
// length, latency) and data-protection checks (no PII in the reply, DLP
// redacted or blocked the prompt). It backs `bairav eval run` and the
// `--eval` gate on `bairav model upgrade`.
package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bairav-foundation/orch-engine/cli/internal/dlp"
)

// Suite is one eval file.
type Suite struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description,omitempty"`
	// NeedsDLP marks a suite that checks a DLP proxy, not the model: it
	// only passes when --endpoint points at the proxy.
	NeedsDLP bool   `yaml:"needs_dlp" json:"needs_dlp,omitempty"`
	Model    string `yaml:"model" json:"model,omitempty"`   // request "model" field; most local servers ignore it
	System   string `yaml:"system" json:"system,omitempty"` // default system prompt for every case
	// Threshold is the share of cases (0..1) that must pass for the suite
	// to pass. Default 1: every case.
	Threshold *float64 `yaml:"threshold" json:"threshold,omitempty"`
	Defaults  Defaults `yaml:"defaults" json:"defaults"`
	Cases     []Case   `yaml:"cases" json:"cases"`
}

// Defaults apply to every case unless the case overrides them.
type Defaults struct {
	Temperature *float64 `yaml:"temperature" json:"temperature,omitempty"` // default 0
	MaxTokens   int      `yaml:"max_tokens" json:"max_tokens,omitempty"`   // default 256
	Repeat      int      `yaml:"repeat" json:"repeat,omitempty"`           // default 1
	TimeoutSec  int      `yaml:"timeout_seconds" json:"timeout_seconds,omitempty"`
}

// Case is one prompt and what the reply must look like.
type Case struct {
	Name   string `yaml:"name" json:"name"`
	Prompt string `yaml:"prompt" json:"prompt"`
	System string `yaml:"system" json:"system,omitempty"`
	// Repeat sends the prompt N times; MinPass of them must pass (default
	// all). Useful with temperature > 0 or flaky small models.
	Repeat    int      `yaml:"repeat" json:"repeat,omitempty"`
	MinPass   int      `yaml:"min_pass" json:"min_pass,omitempty"`
	MaxTokens int      `yaml:"max_tokens" json:"max_tokens,omitempty"`
	Expect    Expect   `yaml:"expect" json:"expect"`
	Tags      []string `yaml:"tags" json:"tags,omitempty"`
}

// Expect lists the checks on a reply. Every set field must hold.
type Expect struct {
	Contains    []string `yaml:"contains" json:"contains,omitempty"`         // case-insensitive
	NotContains []string `yaml:"not_contains" json:"not_contains,omitempty"` // case-insensitive
	Regex       []string `yaml:"regex" json:"regex,omitempty"`
	NotRegex    []string `yaml:"not_regex" json:"not_regex,omitempty"`
	Equals      string   `yaml:"equals" json:"equals,omitempty"` // after trimming spaces
	JSON        bool     `yaml:"json" json:"json,omitempty"`     // reply parses as JSON (a ```json fence is allowed)
	MaxChars    int      `yaml:"max_chars" json:"max_chars,omitempty"`
	MaxLatency  int      `yaml:"max_latency_ms" json:"max_latency_ms,omitempty"`

	// NoPII fails the case if the DLP detectors find anything in the reply.
	NoPII bool `yaml:"no_pii" json:"no_pii,omitempty"`
	// Redacted lists labels that must appear as [REDACTED:LABEL] in the
	// reply: proof that a DLP proxy in front of the model rewrote the prompt.
	Redacted []string `yaml:"redacted" json:"redacted,omitempty"`
	// Blocked expects the endpoint to refuse the prompt with HTTP 403
	// (what a DLP proxy with action block returns).
	Blocked bool `yaml:"blocked" json:"blocked,omitempty"`
}

// Load reads and validates a suite file.
func Load(path string) (*Suite, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b, path)
}

// Parse validates a suite from bytes. name is used in error messages.
func Parse(b []byte, name string) (*Suite, error) {
	var s Suite
	if err := yaml.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if s.Name == "" {
		s.Name = strings.TrimSuffix(strings.TrimSuffix(baseName(name), ".yaml"), ".yml")
	}
	if len(s.Cases) == 0 {
		return nil, fmt.Errorf("%s: no cases", name)
	}
	if s.Threshold != nil && (*s.Threshold < 0 || *s.Threshold > 1) {
		return nil, fmt.Errorf("%s: threshold must be between 0 and 1 (got %v)", name, *s.Threshold)
	}
	seen := map[string]bool{}
	for i := range s.Cases {
		c := &s.Cases[i]
		if c.Name == "" {
			c.Name = fmt.Sprintf("case-%d", i+1)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("%s: duplicate case name %q", name, c.Name)
		}
		seen[c.Name] = true
		if strings.TrimSpace(c.Prompt) == "" {
			return nil, fmt.Errorf("%s: case %q has no prompt", name, c.Name)
		}
		for _, re := range append(append([]string{}, c.Expect.Regex...), c.Expect.NotRegex...) {
			if _, err := regexp.Compile(re); err != nil {
				return nil, fmt.Errorf("%s: case %q: bad regex %q: %v", name, c.Name, re, err)
			}
		}
		if c.Repeat == 0 {
			c.Repeat = s.Defaults.Repeat
		}
		if c.Repeat <= 0 {
			c.Repeat = 1
		}
		if c.MinPass == 0 || c.MinPass > c.Repeat {
			if c.MinPass > c.Repeat {
				return nil, fmt.Errorf("%s: case %q: min_pass %d is more than repeat %d", name, c.Name, c.MinPass, c.Repeat)
			}
			c.MinPass = c.Repeat
		}
	}
	return &s, nil
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ThresholdOrDefault is the pass share the suite needs.
func (s *Suite) ThresholdOrDefault() float64 {
	if s.Threshold == nil {
		return 1
	}
	return *s.Threshold
}

// Poster sends one chat-completions body and returns the HTTP status and
// raw response. HTTPPoster covers normal endpoints; the CLI supplies one
// that goes through the Kubernetes API server for in-cluster models.
type Poster func(ctx context.Context, body []byte) (status int, raw []byte, err error)

// HTTPPoster posts to <baseURL>/chat/completions. apiKey is optional.
func HTTPPoster(baseURL, apiKey string, client *http.Client) Poster {
	if client == nil {
		client = http.DefaultClient
	}
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	return func(ctx context.Context, body []byte) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return resp.StatusCode, raw, err
	}
}

// Attempt is one request/response.
type Attempt struct {
	Reply     string   `json:"reply"`
	Status    int      `json:"status"`
	LatencyMS int64    `json:"latency_ms"`
	Failures  []string `json:"failures,omitempty"`
}

// Passed reports whether every check held.
func (a Attempt) Passed() bool { return len(a.Failures) == 0 }

// CaseResult is a case with all its attempts.
type CaseResult struct {
	Name     string    `json:"name"`
	Pass     bool      `json:"pass"`
	Passed   int       `json:"passed"`
	Repeat   int       `json:"repeat"`
	MinPass  int       `json:"min_pass"`
	P50MS    int64     `json:"p50_ms"`
	Attempts []Attempt `json:"attempts"`
}

// Report is the outcome of one suite run against one endpoint.
type Report struct {
	Suite     string       `json:"suite"`
	Target    string       `json:"target"`
	Model     string       `json:"model,omitempty"`
	Started   time.Time    `json:"started"`
	Seconds   float64      `json:"seconds"`
	Passed    int          `json:"passed"`
	Total     int          `json:"total"`
	Score     float64      `json:"score"`
	Threshold float64      `json:"threshold"`
	Pass      bool         `json:"pass"`
	Cases     []CaseResult `json:"cases"`
}

// Options tune a run.
type Options struct {
	Target    string         // label for the report, e.g. the endpoint URL
	Model     string         // overrides the suite's model
	Detectors []dlp.Detector // for no_pii; nil = dlp.DefaultDetectors()
	OnCase    func(CaseResult)
	Now       func() time.Time
}

// Run sends every case through post and scores the replies. Cases run one
// at a time: local models serve one request at a time anyway, and it keeps
// latency numbers honest.
func Run(ctx context.Context, s *Suite, post Poster, o Options) *Report {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	dets := o.Detectors
	if dets == nil {
		dets = dlp.DefaultDetectors()
	}
	model := o.Model
	if model == "" {
		model = s.Model
	}
	temp := 0.0
	if s.Defaults.Temperature != nil {
		temp = *s.Defaults.Temperature
	}
	timeout := time.Duration(s.Defaults.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	r := &Report{Suite: s.Name, Target: o.Target, Model: model, Started: now(), Threshold: s.ThresholdOrDefault()}
	for _, c := range s.Cases {
		res := CaseResult{Name: c.Name, Repeat: c.Repeat, MinPass: c.MinPass}
		system := c.System
		if system == "" {
			system = s.System
		}
		maxTok := c.MaxTokens
		if maxTok == 0 {
			maxTok = s.Defaults.MaxTokens
		}
		if maxTok == 0 {
			maxTok = 256
		}
		body := requestBody(model, system, c.Prompt, temp, maxTok)
		var lat []int64
		for i := 0; i < c.Repeat; i++ {
			if ctx.Err() != nil {
				res.Attempts = append(res.Attempts, Attempt{Failures: []string{"cancelled"}})
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			start := now()
			status, raw, err := post(cctx, body)
			ms := now().Sub(start).Milliseconds()
			cancel()
			a := Attempt{Status: status, LatencyMS: ms}
			if err != nil {
				a.Failures = []string{"request failed: " + err.Error()}
			} else {
				a.Reply, a.Failures = judge(c.Expect, status, raw, ms, dets)
			}
			if a.Passed() {
				res.Passed++
			}
			lat = append(lat, ms)
			res.Attempts = append(res.Attempts, a)
		}
		res.Pass = res.Passed >= c.MinPass
		res.P50MS = median(lat)
		if res.Pass {
			r.Passed++
		}
		r.Cases = append(r.Cases, res)
		if o.OnCase != nil {
			o.OnCase(res)
		}
	}
	r.Total = len(r.Cases)
	if r.Total > 0 {
		r.Score = float64(r.Passed) / float64(r.Total)
	}
	r.Pass = r.Total > 0 && r.Score+1e-9 >= r.Threshold
	r.Seconds = now().Sub(r.Started).Seconds()
	return r
}

func requestBody(model, system, prompt string, temp float64, maxTokens int) []byte {
	var msgs []map[string]string
	if system != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": prompt})
	body := map[string]any{
		"messages":    msgs,
		"temperature": temp, // always sent: servers default to ~0.8, which makes evals noisy
		"max_tokens":  maxTokens,
	}
	if model != "" {
		body["model"] = model
	}
	b, _ := json.Marshal(body)
	return b
}

// judge applies the checks to one response. It returns the reply text (or
// the error body when there is no reply) and the failed checks.
func judge(e Expect, status int, raw []byte, latencyMS int64, dets []dlp.Detector) (string, []string) {
	var fails []string
	fail := func(f string, args ...any) { fails = append(fails, fmt.Sprintf(f, args...)) }

	if e.Blocked {
		if status != http.StatusForbidden {
			fail("expected the prompt to be blocked (HTTP 403), got HTTP %d", status)
		}
		return errorText(raw), fails
	}
	if status != http.StatusOK {
		fail("HTTP %d: %s", status, clip(errorText(raw), 160))
		return errorText(raw), fails
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Choices) == 0 {
		fail("not an OpenAI-style reply: %s", clip(string(raw), 160))
		return "", fails
	}
	reply := resp.Choices[0].Message.Content
	lower := strings.ToLower(reply)

	for _, s := range e.Contains {
		if !strings.Contains(lower, strings.ToLower(s)) {
			fail("missing %q", s)
		}
	}
	for _, s := range e.NotContains {
		if strings.Contains(lower, strings.ToLower(s)) {
			fail("contains %q", s)
		}
	}
	for _, re := range e.Regex {
		if !regexp.MustCompile(re).MatchString(reply) {
			fail("no match for /%s/", re)
		}
	}
	for _, re := range e.NotRegex {
		if regexp.MustCompile(re).MatchString(reply) {
			fail("matches /%s/", re)
		}
	}
	if e.Equals != "" && strings.TrimSpace(reply) != strings.TrimSpace(e.Equals) {
		fail("want exactly %q", clip(e.Equals, 60))
	}
	if e.JSON {
		if !json.Valid([]byte(stripFence(reply))) {
			fail("reply is not valid JSON: %q", clip(reply, 60))
		}
	}
	if e.MaxChars > 0 && len([]rune(reply)) > e.MaxChars {
		fail("%d chars, max %d", len([]rune(reply)), e.MaxChars)
	}
	if e.MaxLatency > 0 && latencyMS > int64(e.MaxLatency) {
		fail("took %d ms, max %d", latencyMS, e.MaxLatency)
	}
	if e.NoPII {
		if hits := dlp.Scan(reply, dets); len(hits) > 0 {
			fail("reply contains %s", strings.Join(dlp.Labels(hits), ", "))
		}
	}
	for _, l := range e.Redacted {
		if !strings.Contains(reply, "[REDACTED:"+strings.ToUpper(l)+"]") {
			fail("no [REDACTED:%s] in the reply", strings.ToUpper(l))
		}
	}
	return reply, fails
}

// stripFence returns what's inside a leading ```json ... ``` block, which
// models add even when asked not to. A note after the closing fence is
// ignored (small models like to explain their JSON); text before the fence
// is not, so "Here is the JSON: ..." still fails.
func stripFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	t = strings.TrimPrefix(t, "```")
	// Optional language tag, on its own line or not: ```json {"a":1}```
	t = strings.TrimLeft(t, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	if end := strings.Index(t, "```"); end >= 0 {
		t = t[:end]
	}
	return strings.TrimSpace(t)
}

func errorText(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return strings.TrimSpace(string(raw))
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// Clip shortens s to n runes on one line, for tables.
func Clip(s string, n int) string { return clip(s, n) }
