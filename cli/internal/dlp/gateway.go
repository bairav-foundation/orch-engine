// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

// gateway mode. The same proxy that guards a local model can
// front a cloud provider (OpenAI, Anthropic, or any OpenAI-compatible
// endpoint). Agents keep calling one OpenAI-style URL, and the gateway:
//
//   - holds the provider API key. Callers never see it, and any key a
//     caller sends is stripped, so the gateway is the only door to the
//     provider and every request is DLP-inspected and audited
//   - pins the model, so callers can't pick a different (pricier) model,
//     and swapping provider/model is a server-side change
//   - allows inference paths only (no /files, /fine_tuning, admin APIs)
//   - fails closed: a body it can't parse is rejected, not forwarded
//   - speaks Anthropic's native Messages API too (/v1/messages), with
//     request, response and streaming inspection
//
// OpenAI-format requests to Anthropic use Anthropic's OpenAI-compatible
// endpoint (api.anthropic.com/v1/chat/completions). Anthropic documents
// that layer as meant mainly for testing and comparison, so production
// Anthropic agents should prefer /v1/messages.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Provider names accepted in Config.Provider.
const (
	ProviderOpenAI           = "openai"
	ProviderAnthropic        = "anthropic"
	ProviderOpenAICompatible = "openai-compatible"
)

// AnthropicVersion is sent on /v1/messages when the caller didn't set one.
const AnthropicVersion = "2023-06-01"

// ProviderBaseURL returns the default API base URL for a provider, or ""
// when the provider has none (openai-compatible needs an explicit URL).
func ProviderBaseURL(provider string) string {
	switch provider {
	case ProviderOpenAI:
		return "https://api.openai.com/v1"
	case ProviderAnthropic:
		return "https://api.anthropic.com/v1"
	}
	return ""
}

// ValidProvider reports whether p is a provider the gateway understands.
func ValidProvider(p string) bool {
	return p == ProviderOpenAI || p == ProviderAnthropic || p == ProviderOpenAICompatible
}

// gatewayPaths are the only paths a gateway forwards. Anything else gets
// a 404, so a leaked gateway URL can't be used to reach the provider's
// file, fine-tuning, batch or admin APIs with our key.
var gatewayPaths = []string{
	"/chat/completions", "/completions", "/embeddings",
	"/messages", "/messages/count_tokens", "/responses",
}

// gatewayMode reports whether this proxy fronts a cloud provider.
func (p *Proxy) gatewayMode() bool { return p.cfg.Provider != "" }

// gatewayAllowed reports whether a gateway may forward this path.
func gatewayAllowed(path string) bool {
	for _, s := range gatewayPaths {
		if strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}

// handleLocal answers requests the gateway serves itself. Returns true
// when the request was handled.
//
// (/healthz and, in gateway mode, /health are answered by serveHealth.)
//
//	GET /v1/models      gateway mode: the pinned model only. Callers learn
//	                    nothing about the provider account.
func (p *Proxy) handleLocal(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	switch {
	case p.gatewayMode() && r.Method == http.MethodGet && strings.HasSuffix(path, "/models"):
		data := []map[string]string{}
		if p.cfg.Model != "" {
			data = append(data, map[string]string{"id": p.cfg.Model, "object": "model", "owned_by": p.cfg.Provider})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
		return true
	case p.gatewayMode() && !gatewayAllowed(path):
		writeJSONError(w, http.StatusNotFound, "path_not_allowed",
			fmt.Sprintf("bairav gateway: %s is not an inference path (allowed: %s)", path, strings.Join(gatewayPaths, ", ")))
		return true
	}
	return false
}

// stripProviderHeaders drops response headers that identify the provider
// account (org / project ids) or set provider cookies; callers of the
// gateway have no business seeing either.
func stripProviderHeaders(h http.Header) {
	for _, k := range []string{"Openai-Organization", "Openai-Project", "Anthropic-Organization-Id", "Set-Cookie"} {
		h.Del(k)
	}
}

// pinModel overwrites the request's "model" field with the configured
// model. Bodies without a JSON object are returned unchanged.
func (p *Proxy) pinModel(body []byte) []byte {
	if p.cfg.Model == "" || len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	var payload map[string]interface{}
	if err := decodeJSON(body, &payload); err != nil {
		return body
	}
	if payload["model"] == p.cfg.Model {
		return body
	}
	payload["model"] = p.cfg.Model
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

// applyProviderAuth replaces whatever credentials the caller sent with
// the gateway's own key, in the header style the provider expects.
func (p *Proxy) applyProviderAuth(h http.Header, path string) {
	if !p.gatewayMode() {
		return
	}
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("Api-Key")
	if p.cfg.Provider == ProviderAnthropic && strings.Contains(path, "/messages") {
		if h.Get("Anthropic-Version") == "" {
			h.Set("Anthropic-Version", AnthropicVersion)
		}
		if p.cfg.APIKey != "" {
			h.Set("X-Api-Key", p.cfg.APIKey)
		}
		return
	}
	if p.cfg.APIKey != "" {
		h.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
}

// -----------------------------------------------------------------------------
// Generic JSON walk: used for formats whose text can sit in many places
// (Anthropic content blocks, OpenAI content parts, Responses API items,
// tool-call arguments). Every string value is scanned except under keys
// that hold identifiers or binary data.
// -----------------------------------------------------------------------------

// walkSkipKeys hold identifiers and enums, never user text. Keep this list
// short and reviewed: every key added here is a field nobody inspects.
// ("name" and "data" used to be here; both can carry user text: a chat
// participant name, an Anthropic text document's body.)
var walkSkipKeys = map[string]bool{
	"model": true, "role": true, "type": true, "id": true, "object": true,
	"tool_use_id": true, "tool_call_id": true, "call_id": true,
	"media_type": true, "signature": true, "stop_reason": true, "finish_reason": true,
}

// isBinaryBlob reports whether a string is an inline base64 payload (image,
// PDF, audio data URI). Such blobs are not text; scanning them wastes time
// and invites false positives. Their contents are NOT inspected.
func isBinaryBlob(s string) bool {
	return strings.HasPrefix(s, "data:") && strings.Contains(s[:min(len(s), 100)], ";base64,")
}

// walk scans and (where the effective action says so) redacts every
// string under v, in place. responseSide applies response semantics:
// everything but audit_only is redacted (see inspectResponse).
func (p *Proxy) walk(v interface{}, responseSide bool, hits *[]Hit, changed *bool) interface{} {
	switch t := v.(type) {
	case string:
		if isBinaryBlob(t) {
			return t
		}
		hs := Scan(t, p.detectors)
		if len(hs) == 0 {
			return t
		}
		*hits = append(*hits, hs...)
		var red []Hit
		for _, h := range hs {
			a := effectiveAction(h, p.cfg.Action)
			if a == ActionRedact || (responseSide && a == ActionBlock) {
				red = append(red, h)
			}
		}
		if len(red) == 0 {
			return t
		}
		*changed = true
		return Redact(t, red)
	case map[string]interface{}:
		for k, val := range t {
			if walkSkipKeys[k] {
				continue
			}
			// Anthropic {"type":"base64","media_type":...,"data":"..."}:
			// binary source. A {"type":"text","data":"..."} document is
			// scanned like any other text.
			if k == "data" && t["type"] == "base64" {
				continue
			}
			t[k] = p.walk(val, responseSide, hits, changed)
		}
		return t
	case []interface{}:
		for i, val := range t {
			t[i] = p.walk(val, responseSide, hits, changed)
		}
		return t
	}
	return v
}

// walkBody walks selected top-level keys of a JSON object body (all keys
// when keys is nil) and re-marshals when something was redacted.
func (p *Proxy) walkBody(body []byte, responseSide bool, keys ...string) ([]byte, []Hit) {
	var payload map[string]interface{}
	if err := decodeJSON(body, &payload); err != nil {
		return body, nil
	}
	var hits []Hit
	changed := false
	if keys == nil {
		p.walk(payload, responseSide, &hits, &changed)
	} else {
		for _, k := range keys {
			if v, ok := payload[k]; ok {
				payload[k] = p.walk(v, responseSide, &hits, &changed)
			}
		}
	}
	if !changed {
		return body, hits
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body, hits
	}
	return out, hits
}

// inspectResponseMessages handles a non-streaming /v1/messages reply:
// content[] text blocks and tool_use inputs.
func (p *Proxy) inspectResponseMessages(body []byte) ([]byte, []Hit) {
	return p.walkBody(body, true, "content")
}

// validJSONObject reports whether body is a JSON object (gateway mode
// refuses to forward anything it could not inspect).
func validJSONObject(body []byte) bool {
	var v map[string]interface{}
	return json.Unmarshal(body, &v) == nil
}
