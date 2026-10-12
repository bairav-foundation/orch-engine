// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstreamReturning stands up a tiny httptest.Server that answers every
// request with the given status + body. Used by the direction=response
// tests below to give the proxy something to buffer.
func upstreamReturning(t *testing.T, status int, contentType, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

// TestResponseRedaction_ChatCompletions verifies that when direction=response
// and the upstream returns PII in the assistant's message, the proxy
// redacts it before the caller sees it.
func TestResponseRedaction_ChatCompletions(t *testing.T) {
	upstream := upstreamReturning(t, 200, "application/json", `{
		"id": "chatcmpl-1",
		"choices": [
			{"index":0, "message": {"role":"assistant","content":"Your ticket handler is jane@corp.com; her ssn is 123-45-6789."}}
		]
	}`)
	defer upstream.Close()

	proxy, err := NewProxy(Config{
		Upstream:  upstream.URL + "/v1",
		Direction: DirectionResponse,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	respBody := rec.Body.String()
	if strings.Contains(respBody, "jane@corp.com") {
		t.Errorf("email leaked in response: %s", respBody)
	}
	if strings.Contains(respBody, "123-45-6789") {
		t.Errorf("SSN leaked in response: %s", respBody)
	}
	if !strings.Contains(respBody, "[REDACTED:EMAIL]") {
		t.Errorf("expected [REDACTED:EMAIL] placeholder: %s", respBody)
	}
	if !strings.Contains(respBody, "[REDACTED:SSN]") {
		t.Errorf("expected [REDACTED:SSN] placeholder: %s", respBody)
	}
	// Content-Length must match the rewritten body (the original code
	// would have kept the upstream's original Content-Length and truncated).
	// Verify by parsing the reply as JSON — a truncation would break it.
	var parsed map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Errorf("rewritten body isn't valid JSON: %v (body: %s)", err, respBody)
	}
}

// TestResponseRedaction_DirectionRequest_LeavesResponseAlone verifies the
// default direction (request) does NOT scan the response side — matches
// request-only behaviour, so nothing existing breaks.
func TestResponseRedaction_DirectionRequest_LeavesResponseAlone(t *testing.T) {
	upstream := upstreamReturning(t, 200, "application/json", `{
		"choices":[{"message":{"role":"assistant","content":"call jane@corp.com"}}]
	}`)
	defer upstream.Close()

	proxy, err := NewProxy(Config{
		Upstream:  upstream.URL + "/v1",
		Direction: DirectionRequest,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "jane@corp.com") {
		t.Errorf("with direction=request, response side should pass through unchanged; body=%s", rec.Body.String())
	}
}

// TestResponseRedaction_SSERedacted: SSE responses are redacted
// incrementally (see stream_test.go for the split-span cases).
func TestResponseRedaction_SSERedacted(t *testing.T) {
	// Emit a couple of SSE frames containing an email.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"reach me at jane@corp.com\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	proxy, err := NewProxy(Config{
		Upstream:  upstream.URL + "/v1",
		Direction: DirectionBoth,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}], "stream": true}`))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "jane@corp.com") {
		t.Errorf("SSE response leaked the email; body=%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[REDACTED:EMAIL]") {
		t.Errorf("expected [REDACTED:EMAIL] in stream; body=%s", rec.Body.String())
	}
}

// TestDirectionAuditFields verifies the audit entry surfaces
// response-side hit counts and labels so SOC dashboards can distinguish
// "PII leaving in prompt" from "PII coming back in completion."
func TestDirectionAuditFields(t *testing.T) {
	upstream := upstreamReturning(t, 200, "application/json", `{
		"choices":[{"message":{"role":"assistant","content":"the ssn is 123-45-6789"}}]
	}`)
	defer upstream.Close()

	var captured []AuditEntry
	audit := &sliceAuditWriter{entries: &captured}

	proxy, err := NewProxy(Config{
		Upstream:  upstream.URL + "/v1",
		Direction: DirectionBoth,
		Audit:     audit,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if len(captured) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(captured))
	}
	e := captured[0]
	if e.Direction != DirectionBoth {
		t.Errorf("Direction = %q, want %q", e.Direction, DirectionBoth)
	}
	if e.ResponseHits == 0 {
		t.Errorf("ResponseHits should be > 0 when SSN appears in the response")
	}
	if !containsLabel(e.ResponseLabels, "SSN") {
		t.Errorf("ResponseLabels missing SSN: %v", e.ResponseLabels)
	}
}

// sliceAuditWriter is a test double that appends every entry to a slice
// the test can inspect. Avoids the need for a bytes.Buffer + JSON parse
// in the common assertion path.
type sliceAuditWriter struct {
	entries *[]AuditEntry
}

func (s *sliceAuditWriter) Write(e AuditEntry) error {
	*s.entries = append(*s.entries, e)
	return nil
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
