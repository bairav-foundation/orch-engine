// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseChat renders text as an OpenAI chat-completions SSE stream, split
// into chunks of chunkSize bytes, ending with a finish_reason frame and
// [DONE]. Deliberately splits mid-token so PII straddles chunks.
func sseChat(text string, chunkSize int) string {
	var sb strings.Builder
	for i := 0; i < len(text); i += chunkSize {
		end := i + chunkSize
		if end > len(text) {
			end = len(text)
		}
		b, _ := json.Marshal(map[string]interface{}{
			"object":  "chat.completion.chunk",
			"choices": []interface{}{map[string]interface{}{"index": 0, "delta": map[string]interface{}{"content": text[i:end]}, "finish_reason": nil}},
		})
		fmt.Fprintf(&sb, "data: %s\n\n", b)
	}
	sb.WriteString(`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// reassembleChat concatenates every delta.content in an SSE body.
func reassembleChat(t *testing.T, body string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(line[6:]) == "[DONE]" {
			continue
		}
		var p struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &p); err != nil {
			t.Fatalf("bad SSE json %q: %v", line, err)
		}
		for _, c := range p.Choices {
			sb.WriteString(c.Delta.Content)
		}
	}
	return sb.String()
}

func streamingUpstream(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, body)
	}))
}

// TestStreamRedaction_SpanSplitAcrossChunks is the core streaming guarantee:
// an email and an SSN split across tiny SSE chunks still get redacted,
// and the reassembled stream equals whole-text redaction.
func TestStreamRedaction_SpanSplitAcrossChunks(t *testing.T) {
	text := "Owner is jane.doe@corp-mail.com and backup ssn 123-45-6789. Thanks!"
	want := Redact(text, Scan(text, DefaultDetectors()))

	for _, size := range []int{1, 3, 7, 16} {
		upstream := streamingUpstream(sseChat(text, size))
		proxy, err := NewProxy(Config{Upstream: upstream.URL + "/v1", Direction: DirectionBoth})
		if err != nil {
			t.Fatalf("NewProxy: %v", err)
		}
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`))
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		upstream.Close()

		got := reassembleChat(t, rec.Body.String())
		if got != want {
			t.Errorf("chunk=%d: reassembled stream\n got: %q\nwant: %q", size, got, want)
		}
		if strings.Contains(rec.Body.String(), "jane.doe@corp-mail.com") || strings.Contains(rec.Body.String(), "123-45-6789") {
			t.Errorf("chunk=%d: raw PII leaked in stream", size)
		}
		if !strings.Contains(rec.Body.String(), "data: [DONE]") {
			t.Errorf("chunk=%d: [DONE] terminator lost", size)
		}
	}
}

// TestStreamRedaction_DirectionRequestPassesThrough — with direction=
// request the stream is untouched (request-only behaviour preserved).
func TestStreamRedaction_DirectionRequestPassesThrough(t *testing.T) {
	upstream := streamingUpstream(sseChat("mail jane@corp.com", 4))
	defer upstream.Close()
	proxy, err := NewProxy(Config{Upstream: upstream.URL + "/v1", Direction: DirectionRequest})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if got := reassembleChat(t, rec.Body.String()); got != "mail jane@corp.com" {
		t.Errorf("direction=request should not touch the stream; got %q", got)
	}
}

// TestStreamRedaction_AuditCounts — each span counted once, not once
// per chunk re-scan.
func TestStreamRedaction_AuditCounts(t *testing.T) {
	upstream := streamingUpstream(sseChat("a@b.io and c@d.io", 2))
	defer upstream.Close()
	var captured []AuditEntry
	proxy, err := NewProxy(Config{
		Upstream: upstream.URL + "/v1", Direction: DirectionResponse,
		Audit: &sliceAuditWriter{entries: &captured},
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	proxy.ServeHTTP(httptest.NewRecorder(), req)
	if len(captured) != 1 {
		t.Fatalf("want 1 audit entry, got %d", len(captured))
	}
	if captured[0].ResponseHits != 2 {
		t.Errorf("ResponseHits = %d, want 2 (one per email, no double counting)", captured[0].ResponseHits)
	}
}

// TestStreamRedaction_NoFinishReasonFlushedAtEOF — upstreams that end
// without finish_reason or [DONE] still deliver every (redacted) byte.
func TestStreamRedaction_NoFinishReasonFlushedAtEOF(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"content":"ssn 123-45-6789 end"},"finish_reason":null}]}` + "\n\n"
	upstream := streamingUpstream(body)
	defer upstream.Close()
	proxy, err := NewProxy(Config{Upstream: upstream.URL + "/v1", Direction: DirectionBoth})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if got := reassembleChat(t, rec.Body.String()); got != "ssn [REDACTED:SSN] end" {
		t.Errorf("got %q, want %q", got, "ssn [REDACTED:SSN] end")
	}
}
