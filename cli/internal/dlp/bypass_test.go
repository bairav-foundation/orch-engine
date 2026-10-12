// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

// Regression suite for the October 2026 external teardown: every bypass it
// reported (and the extra ones found while reproducing it) must stay closed.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const bypassSSN = "123-45-6789"

func TestNoFieldBypasses(t *testing.T) {
	cases := []struct{ name, prov, path, body string }{
		{"chat content", "openai", "/v1/chat/completions", `{"messages":[{"role":"user","content":"SSN ` + bypassSSN + `"}]}`},
		{"anthropic text document data", "anthropic", "/v1/messages", `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"SSN ` + bypassSSN + `"}}]}]}`},
		{"tools description", "openai", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","description":"SSN ` + bypassSSN + `","parameters":{}}}]}`},
		{"responses prompt.variables", "openai", "/v1/responses", `{"prompt":{"id":"p","variables":{"x":"SSN ` + bypassSSN + `"}}}`},
		{"messages[].name", "openai", "/v1/chat/completions", `{"messages":[{"role":"user","name":"` + bypassSSN + `","content":"hi"}]}`},
		{"response_format schema", "openai", "/v1/chat/completions", `{"messages":[],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"description":"SSN ` + bypassSSN + `"}}}}`},
		{"metadata", "openai", "/v1/chat/completions", `{"messages":[],"metadata":{"k":"SSN ` + bypassSSN + `"}}`},
		{"completions suffix", "openai", "/v1/completions", `{"prompt":"x","suffix":"SSN ` + bypassSSN + `"}`},
		{"user field", "openai", "/v1/chat/completions", `{"messages":[],"user":"SSN ` + bypassSSN + `"}`},
		{"anthropic metadata", "anthropic", "/v1/messages", `{"messages":[],"metadata":{"user_id":"` + bypassSSN + `"}}`},
		{"plain proxy, same field", "", "/v1/chat/completions", `{"messages":[],"metadata":{"k":"SSN ` + bypassSSN + `"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newFakeProvider("application/json", `{}`)
			defer up.srv.Close()
			p, err := NewProxy(Config{Upstream: up.srv.URL + "/v1", Provider: c.prov, APIKey: "k", Action: ActionBlock})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(p)
			defer srv.Close()
			resp, _ := post(t, srv.URL+c.path, c.body, nil)
			if calls, _, _, got := up.snapshot(); calls > 0 && strings.Contains(got, bypassSSN) {
				t.Fatalf("PII reached the upstream (status %d)", resp.StatusCode)
			}
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status %d, want 403 (block)", resp.StatusCode)
			}
		})
	}
}

func TestBinaryBlobsNotScanned(t *testing.T) {
	up := newFakeProvider("application/json", `{}`)
	defer up.srv.Close()
	p, _ := NewProxy(Config{Upstream: up.srv.URL + "/v1", Provider: "anthropic", APIKey: "k", Action: ActionBlock})
	srv := httptest.NewServer(p)
	defer srv.Close()
	// Digits inside base64 that happen to look like an SSN must not block an image.
	body := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA` + bypassSSN + `AAA"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA` + bypassSSN + `"}}]}]}`
	if resp, _ := post(t, srv.URL+"/v1/messages", body, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("binary payloads should pass through, got %d", resp.StatusCode)
	}
}

func TestPathVariantsAndUninspectedRoutes(t *testing.T) {
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, strings.TrimSuffix(r.URL.Path, "/"), http.StatusTemporaryRedirect)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer up.Close()
	p, _ := NewProxy(Config{Upstream: up.URL + "/v1", Action: ActionBlock})
	srv := httptest.NewServer(p)
	defer srv.Close()
	pii := `{"messages":[{"role":"user","content":"SSN ` + bypassSSN + `"}]}`
	for _, path := range []string{"/v1/chat/completions/", "/v1//chat/completions", "/v1/Chat/Completions", "/v1/./chat/completions"} {
		got = ""
		if resp, _ := post(t, srv.URL+path, pii, nil); resp.StatusCode != http.StatusForbidden || strings.Contains(got, bypassSSN) {
			t.Errorf("%s: status %d, upstream got PII=%v", path, resp.StatusCode, strings.Contains(got, bypassSSN))
		}
	}
	// llama-server's native routes carry prompts too; they are not inspected, so refused.
	for _, path := range []string{"/completion", "/infill", "/tokenize", "/v1/rerank"} {
		if resp, _ := post(t, srv.URL+path, `{"prompt":"SSN `+bypassSSN+`"}`, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s should be refused, got %d", path, resp.StatusCode)
		}
	}
	got = ""
	if resp, _ := post(t, srv.URL+"/v1/chat/completions", "garbage "+bypassSSN, nil); resp.StatusCode != http.StatusBadRequest || got != "" {
		t.Errorf("unparseable body: status %d, forwarded=%v", resp.StatusCode, got != "")
	}
	// GETs (model list, health of the upstream) still pass.
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/models should pass through: %v %v", err, resp)
	}
}

func TestUpstreamRedirectsNotFollowed(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	defer target.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer up.Close()
	p, _ := NewProxy(Config{Upstream: up.URL + "/v1"})
	srv := httptest.NewServer(p)
	defer srv.Close()
	// The test client itself must not follow either, or it would hit target.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits != 0 || resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("redirect must be returned to the caller, not followed (target hits %d, status %d)", hits, resp.StatusCode)
	}
}

func TestClientTokens(t *testing.T) {
	up := newFakeProvider("application/json", `{}`)
	defer up.srv.Close()
	tok := "agent-token-0123456789abcdef"
	p, _ := NewProxy(Config{Upstream: up.srv.URL + "/v1", Provider: "openai", APIKey: "sk-org", ClientTokens: []string{tok}})
	srv := httptest.NewServer(p)
	defer srv.Close()
	body := `{"messages":[]}`
	for name, hdr := range map[string]map[string]string{
		"none":  nil,
		"wrong": {"Authorization": "Bearer nope-nope-nope-nope"},
	} {
		if resp, _ := post(t, srv.URL+"/v1/chat/completions", body, hdr); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s token: got %d, want 401", name, resp.StatusCode)
		}
	}
	if calls, _, _, _ := up.snapshot(); calls != 0 {
		t.Fatal("unauthenticated request reached the provider")
	}
	for _, hdr := range []map[string]string{{"Authorization": "Bearer " + tok}, {"X-Api-Key": tok}} {
		if resp, _ := post(t, srv.URL+"/v1/chat/completions", body, hdr); resp.StatusCode != http.StatusOK {
			t.Errorf("valid token %v: got %d", hdr, resp.StatusCode)
		}
	}
	// The caller's token is never forwarded; the provider sees the org key.
	if _, _, h, _ := up.snapshot(); h.Get("Authorization") != "Bearer sk-org" || h.Get("X-Api-Key") != "" {
		t.Errorf("caller token leaked upstream: %v", h)
	}
	if resp, err := http.Get(srv.URL + "/healthz"); err != nil || resp.StatusCode != 200 {
		t.Error("/healthz must not need a token (kubelet probes)")
	}
}

type failingAudit struct{}

func (failingAudit) Write(AuditEntry) error { return io.ErrClosedPipe }

func TestAuditFailureIsVisible(t *testing.T) {
	up := newFakeProvider("application/json", `{}`)
	defer up.srv.Close()
	for _, required := range []bool{false, true} {
		p, _ := NewProxy(Config{Upstream: up.srv.URL + "/v1", Audit: failingAudit{}, AuditRequired: required})
		srv := httptest.NewServer(p)
		post(t, srv.URL+"/v1/chat/completions", `{"messages":[]}`, nil)
		// The audit entry is written as the response finishes, which can be
		// a moment after the client has read it; wait briefly.
		for i := 0; i < 100 && p.AuditFailures() != 1; i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if p.AuditFailures() != 1 {
			t.Errorf("required=%v: failure not counted", required)
		}
		h, _ := http.Get(srv.URL + "/healthz")
		next, _ := post(t, srv.URL+"/v1/chat/completions", `{"messages":[]}`, nil)
		if required && (h.StatusCode != 503 || next.StatusCode != 503) {
			t.Errorf("audit required: health %d, next request %d; want 503/503", h.StatusCode, next.StatusCode)
		}
		if !required && (h.StatusCode != 200 || next.StatusCode != 200) {
			t.Errorf("audit optional: health %d, next %d; want 200/200", h.StatusCode, next.StatusCode)
		}
		srv.Close()
	}
}
