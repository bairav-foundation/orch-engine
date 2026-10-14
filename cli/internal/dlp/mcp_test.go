// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMCP is a minimal MCP server with both transports:
//   - legacy SSE: GET /sse (endpoint event, then messages), POST /messages → 202
//   - streamable HTTP: POST /mcp → JSON reply, or SSE when Accept asks for it
//
// The echo tool returns its text plus a "secret" it found on the server,
// so the response side has something to catch.
type fakeMCP struct {
	mu       sync.Mutex
	gotBody  []string
	outbox   chan string
	selfURL  string
	healthOK atomic.Bool
}

// lockedAudit is safe for the proxy's concurrent writers (SSE events are
// audited from the streaming goroutine).
type lockedAudit struct {
	mu      sync.Mutex
	entries []AuditEntry
}

func (a *lockedAudit) Write(e AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func (a *lockedAudit) all() []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditEntry(nil), a.entries...)
}

func (f *fakeMCP) reply(body []byte) string {
	var msg struct {
		ID     interface{} `json:"id"`
		Params struct {
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &msg)
	out, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text",
			"text": "echo: " + msg.Params.Arguments.Text + " / config owner ops@corp.example"}},
	}})
	return string(out)
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/health":
		if !f.healthOK.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	case r.Method == http.MethodGet && r.URL.Path == "/sse":
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		// Absolute URL on purpose: the proxy must turn it back into a path.
		fmt.Fprintf(w, "event: endpoint\ndata: %s/messages?session_id=abc\n\n", f.selfURL)
		fl.Flush()
		for {
			select {
			case m := <-f.outbox:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", m)
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	case r.Method == http.MethodPost && r.URL.Path == "/messages":
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.gotBody = append(f.gotBody, string(b))
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		f.outbox <- f.reply(b)
	case r.Method == http.MethodPost && r.URL.Path == "/mcp":
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.gotBody = append(f.gotBody, string(b))
		f.mu.Unlock()
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", f.reply(b))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.reply(b))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeMCP) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.gotBody) == 0 {
		return ""
	}
	return f.gotBody[len(f.gotBody)-1]
}

func newMCPPair(t *testing.T, action Action) (*fakeMCP, *httptest.Server, *lockedAudit) {
	t.Helper()
	f := &fakeMCP{outbox: make(chan string, 4)}
	f.healthOK.Store(true)
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	f.selfURL = up.URL
	audit := &lockedAudit{}
	mp, err := NewMCPProxy(Config{Upstream: up.URL, Action: action, Audit: audit}, MCPOptions{UpstreamHealth: "/health"})
	if err != nil {
		t.Fatal(err)
	}
	px := httptest.NewServer(mp)
	t.Cleanup(px.Close)
	return f, px, audit
}

const toolCall = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"text":"key AKIAIOSFODNN7EXAMPLE"}}}`

func TestMCPStreamableJSON(t *testing.T) {
	f, px, audit := newMCPPair(t, ActionRedact)
	resp, err := http.Post(px.URL+"/mcp", "application/json", strings.NewReader(toolCall))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(f.lastBody(), "AKIA") || !strings.Contains(f.lastBody(), "[REDACTED:AWS_ACCESS_KEY]") {
		t.Errorf("tool arguments not redacted on the way out: %s", f.lastBody())
	}
	if !strings.Contains(f.lastBody(), `"id":7`) {
		t.Errorf("JSON-RPC id must survive: %s", f.lastBody())
	}
	if strings.Contains(string(b), "ops@corp.example") || !strings.Contains(string(b), "[REDACTED:EMAIL]") {
		t.Errorf("tool result not redacted on the way back: %s", b)
	}
	if es := audit.all(); len(es) != 1 || es[0].HitCount != 1 || es[0].ResponseHits != 1 {
		t.Errorf("audit: %+v", es)
	}
}

func TestMCPStreamableSSE(t *testing.T) {
	_, px, _ := newMCPPair(t, ActionRedact)
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/mcp", strings.NewReader(toolCall))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "ops@corp.example") || !strings.Contains(string(b), "[REDACTED:EMAIL]") {
		t.Errorf("SSE tool result not redacted: %s", b)
	}
	if !strings.HasPrefix(string(b), "event: message\ndata: {") {
		t.Errorf("SSE framing broken: %q", b)
	}
}

func TestMCPLegacySSE(t *testing.T) {
	f, px, audit := newMCPPair(t, ActionRedact)
	resp, err := http.Get(px.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readEvent := func() string {
		var lines []string
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended: %v", err)
			}
			if l == "\n" {
				return strings.Join(lines, "")
			}
			lines = append(lines, l)
		}
	}
	ep := readEvent()
	if strings.Contains(ep, "http://") || !strings.Contains(ep, "data: /messages?session_id=abc") {
		t.Fatalf("endpoint event should point back through the proxy: %q", ep)
	}
	post, err := http.Post(px.URL+"/messages?session_id=abc", "application/json", strings.NewReader(toolCall))
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusAccepted {
		t.Fatalf("POST: %d", post.StatusCode)
	}
	if strings.Contains(f.lastBody(), "AKIA") {
		t.Errorf("arguments leaked: %s", f.lastBody())
	}
	done := make(chan string, 1)
	go func() { done <- readEvent() }()
	select {
	case ev := <-done:
		if strings.Contains(ev, "ops@corp.example") || !strings.Contains(ev, "[REDACTED:EMAIL]") {
			t.Errorf("SSE result not redacted: %q", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message event")
	}
	found := false
	for _, e := range audit.all() {
		if e.Method == "SSE" && e.ResponseHits == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("SSE hit not audited: %+v", audit.all())
	}
}

func TestMCPBlock(t *testing.T) {
	f, px, _ := newMCPPair(t, ActionBlock)
	resp, err := http.Post(px.URL+"/mcp", "application/json", strings.NewReader(toolCall))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || out.ID != 7 || out.Error.Code != MCPBlockedCode || !strings.Contains(out.Error.Message, "AWS_ACCESS_KEY") {
		t.Errorf("block reply: %d %+v", resp.StatusCode, out)
	}
	if f.lastBody() != "" {
		t.Errorf("blocked call reached the server: %s", f.lastBody())
	}
}

func TestMCPRefusesNonJSON(t *testing.T) {
	f, px, _ := newMCPPair(t, ActionRedact)
	for _, body := range []string{"not json AKIAIOSFODNN7EXAMPLE", `"just a string"`} {
		resp, err := http.Post(px.URL+"/anything", "text/plain", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", body, resp.StatusCode)
		}
	}
	if f.lastBody() != "" {
		t.Errorf("unparseable body forwarded: %s", f.lastBody())
	}
}

func TestMCPHealthFollowsUpstream(t *testing.T) {
	f, px, _ := newMCPPair(t, ActionRedact)
	get := func() int {
		resp, err := http.Get(px.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get(); c != 200 {
		t.Errorf("healthy upstream: %d", c)
	}
	f.healthOK.Store(false)
	if c := get(); c != http.StatusServiceUnavailable {
		t.Errorf("unhealthy upstream: %d", c)
	}
}

func TestMCPBlockReplyBatch(t *testing.T) {
	var batch interface{}
	_ = json.Unmarshal([]byte(`[{"id":1,"method":"a"},{"method":"notify"},{"id":"x","method":"b"}]`), &batch)
	out, _ := json.Marshal(mcpBlockReply(batch, "no"))
	if strings.Count(string(out), `"error"`) != 2 {
		t.Errorf("one error per request with an id: %s", out)
	}
}

func TestMCPUpstreamCredential(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer up.Close()
	mp, err := NewMCPProxy(Config{Upstream: up.URL + "/mcp"}, MCPOptions{
		UpstreamHeaders: http.Header{"Authorization": {"Bearer remote-secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	px := httptest.NewServer(mp)
	defer px.Close()
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer caller-token")
	req.Header.Set("X-Api-Key", "caller-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("Authorization") != "Bearer remote-secret" || got.Get("X-Api-Key") != "" {
		t.Errorf("upstream saw %v", got)
	}
}
