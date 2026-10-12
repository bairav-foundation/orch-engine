// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

// LiteLLM guardrail mode.
//
// LiteLLM's "Generic Guardrail API" lets any HTTP service act as a
// guardrail: LiteLLM extracts the texts from a request (or response),
// POSTs them to /beta/litellm_basic_guardrail_api, and applies the
// verdict. This file implements that endpoint on the same detectors,
// packs, policies, actions and audit log as the proxy, so a team that
// already routes traffic through LiteLLM can add BAIRAVS DLP with a few
// lines of LiteLLM config and no change to their traffic path.
//
// Contract: https://docs.litellm.ai/docs/adding_provider/generic_guardrail_api
//
//	block hit anywhere                -> BLOCKED (with the labels as reason)
//	redact hit in texts               -> GUARDRAIL_INTERVENED, texts rewritten 1:1
//	redact hit in tool_call arguments -> BLOCKED (the API can't rewrite them;
//	                                     passing them through would leak)
//	audit_only hits / no hits         -> NONE
//
// Images aren't inspected (pattern detectors work on text).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LiteLLMGuardrailPath is the endpoint LiteLLM's generic_guardrail_api calls.
const LiteLLMGuardrailPath = "/beta/litellm_basic_guardrail_api"

// LiteLLM verdicts.
const (
	guardrailNone         = "NONE"
	guardrailBlocked      = "BLOCKED"
	guardrailIntervened   = "GUARDRAIL_INTERVENED"
	guardrailInputRspSide = "response"
)

type guardrailToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type guardrailRequest struct {
	Texts     []string            `json:"texts"`
	ToolCalls []guardrailToolCall `json:"tool_calls"`
	InputType string              `json:"input_type"`
	CallID    string              `json:"litellm_call_id"`
}

type guardrailResponse struct {
	Action        string   `json:"action"`
	BlockedReason string   `json:"blocked_reason,omitempty"`
	Texts         []string `json:"texts,omitempty"`
}

// Guardrail serves the LiteLLM Generic Guardrail API.
type Guardrail struct {
	p *Proxy // detectors, action, audit, tokens and health, shared with the proxy
}

// NewGuardrail builds a guardrail endpoint from the same Config the proxy
// uses. Upstream, Provider, APIKey and Model are ignored: nothing is
// forwarded anywhere.
func NewGuardrail(cfg Config) *Guardrail {
	if cfg.Detectors == nil {
		cfg.Detectors = DefaultDetectors()
	}
	if cfg.Action == "" {
		cfg.Action = ActionRedact
	}
	cfg.Direction = DirectionBoth
	return &Guardrail{p: &Proxy{cfg: cfg, detectors: cfg.Detectors, direction: DirectionBoth}}
}

// AuditFailures is the number of audit records that could not be written.
func (g *Guardrail) AuditFailures() int64 { return g.p.AuditFailures() }

// ServeHTTP implements http.Handler.
func (g *Guardrail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	p := g.p
	path := canonicalPath(r.URL.Path)
	if path == "/healthz" || path == "/health" {
		p.serveHealth(w)
		return
	}
	if path != LiteLLMGuardrailPath || r.Method != http.MethodPost {
		writeJSONError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("bairav DLP guardrail: only POST %s is served", LiteLLMGuardrailPath))
		return
	}
	if !p.authorized(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized",
			"bairav: missing or invalid token (Authorization: Bearer <token>)")
		return
	}
	if p.cfg.AuditRequired && p.auditFailures.Load() > 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "audit_unavailable",
			"bairav: audit log is failing and audit is required; refusing requests")
		return
	}
	limit := p.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "dlp_body_too_large",
				fmt.Sprintf("request body over %d bytes", limit))
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	var req guardrailRequest
	if err := decodeJSON(body, &req); err != nil {
		// Fail closed: LiteLLM treats a non-2xx as a guardrail error, and
		// its default (fail_on_error: true) blocks the call.
		writeJSONError(w, http.StatusBadRequest, "dlp_unparseable", "bairav DLP guardrail: body is not valid JSON")
		return
	}

	resp, hits := g.decide(req)

	dir := DirectionRequest
	if req.InputType == guardrailInputRspSide {
		dir = DirectionResponse
	}
	entry := AuditEntry{
		Timestamp:    started.UTC(),
		Method:       r.Method,
		Path:         LiteLLMGuardrailPath,
		Direction:    dir,
		Action:       overallAction(hits, p.cfg.Action),
		Labels:       Labels(hits),
		HitCount:     len(hits),
		LabelActions: labelActionMap(hits, p.cfg.Action),
		Matches:      p.auditMatches(hits),
		LatencyMS:    time.Since(started).Milliseconds(),
	}
	if dir == DirectionResponse {
		entry.ResponseHits, entry.ResponseLabels = len(hits), Labels(hits)
	}
	p.writeAudit(entry)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// decide scans texts and tool-call arguments and returns LiteLLM's verdict.
func (g *Guardrail) decide(req guardrailRequest) (guardrailResponse, []Hit) {
	global := g.p.cfg.Action
	var all []Hit

	texts := make([]string, len(req.Texts))
	changed := false
	for i, t := range req.Texts {
		texts[i] = t
		hs := Scan(t, g.p.detectors)
		if len(hs) == 0 {
			continue
		}
		all = append(all, hs...)
		var red []Hit
		for _, h := range hs {
			if effectiveAction(h, global) == ActionRedact {
				red = append(red, h)
			}
		}
		if len(red) > 0 {
			texts[i] = Redact(t, red)
			changed = true
		}
	}

	var toolRedact []string
	for _, tc := range req.ToolCalls {
		hs := Scan(tc.Function.Arguments, g.p.detectors)
		all = append(all, hs...)
		for _, h := range hs {
			if effectiveAction(h, global) == ActionRedact {
				toolRedact = append(toolRedact, h.Label)
			}
		}
	}

	if overallAction(all, global) == ActionBlock {
		return guardrailResponse{Action: guardrailBlocked,
			BlockedReason: "Blocked by BAIRAVS DLP: detected " + strings.Join(blockLabels(all, global), ", ")}, all
	}
	if len(toolRedact) > 0 {
		return guardrailResponse{Action: guardrailBlocked,
			BlockedReason: "Blocked by BAIRAVS DLP: sensitive data in tool call arguments (" +
				strings.Join(uniq(toolRedact), ", ") + "), which this API can't redact"}, all
	}
	if changed {
		return guardrailResponse{Action: guardrailIntervened, Texts: texts}, all
	}
	return guardrailResponse{Action: guardrailNone}, all
}

// blockLabels lists the labels whose effective action is block.
func blockLabels(hits []Hit, global Action) []string {
	var out []string
	for _, h := range hits {
		if effectiveAction(h, global) == ActionBlock {
			out = append(out, h.Label)
		}
	}
	return uniq(out)
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
