// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeProvider records what the gateway forwarded and replies with a
// canned body (JSON or SSE).
type fakeProvider struct {
	srv         *httptest.Server
	mu          sync.Mutex
	calls       int
	path        string
	header      http.Header
	body        []byte
	contentType string
	reply       string
}

func newFakeProvider(contentType, reply string) *fakeProvider {
	f := &fakeProvider{contentType: contentType, reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls++
		f.path, f.header, f.body = r.URL.Path, r.Header.Clone(), b
		f.mu.Unlock()
		w.Header().Set("Content-Type", f.contentType)
		_, _ = io.WriteString(w, f.reply)
	}))
	return f
}

func (f *fakeProvider) snapshot() (int, string, http.Header, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.path, f.header, string(f.body)
}

func newGateway(t *testing.T, provider string, up *fakeProvider, dir Direction) (*httptest.Server, *collectingAudit) {
	t.Helper()
	audit := newCollectingAudit()
	p, err := NewProxy(Config{Upstream: up.srv.URL + "/v1", Provider: provider, APIKey: "sk-gateway-secret",
		Model: "pinned-model", Audit: audit, Direction: dir})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(p), audit
}

func post(t *testing.T, url, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestGateway_OpenAIKeyModelAndRedaction(t *testing.T) {
	up := newFakeProvider("application/json", `{"choices":[{"message":{"content":"ok"}}]}`)
	defer up.srv.Close()
	gw, audit := newGateway(t, ProviderOpenAI, up, DirectionRequest)
	defer gw.Close()

	body := `{"model":"gpt-whatever-the-agent-says","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"mail alice@example.com"}]},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"ssn\":\"123-45-6789\"}"}}]}]}`
	resp, _ := post(t, gw.URL+"/v1/chat/completions", body, map[string]string{
		"Authorization": "Bearer sk-callers-own-key", "Accept-Encoding": "br"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	audit.next(t)
	_, path, h, got := up.snapshot()
	if path != "/v1/chat/completions" {
		t.Errorf("path %q", path)
	}
	if h.Get("Authorization") != "Bearer sk-gateway-secret" {
		t.Errorf("gateway key not injected / caller key not stripped: %q", h.Get("Authorization"))
	}
	if h.Get("Accept-Encoding") == "br" {
		t.Error("caller Accept-Encoding forwarded: response DLP could get an undecodable body")
	}
	if !strings.Contains(got, `"model":"pinned-model"`) {
		t.Errorf("model not pinned: %s", got)
	}
	if strings.Contains(got, "alice@example.com") || strings.Contains(got, "123-45-6789") {
		t.Errorf("PII in content parts / tool_call args reached the provider: %s", got)
	}
}

func TestGateway_AnthropicMessages(t *testing.T) {
	reply := `{"type":"message","role":"assistant","content":[{"type":"text","text":"Contact bob@example.org"}],"stop_reason":"end_turn"}`
	up := newFakeProvider("application/json", reply)
	defer up.srv.Close()
	gw, audit := newGateway(t, ProviderAnthropic, up, DirectionBoth)
	defer gw.Close()

	body := `{"model":"claude-x","max_tokens":64,"system":"Customer email: carol@example.com",` +
		`"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"SSN 123-45-6789"}]}]}`
	resp, out := post(t, gw.URL+"/v1/messages", body, map[string]string{"X-Api-Key": "sk-ant-callers"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	e := audit.next(t)
	_, _, h, got := up.snapshot()
	if h.Get("X-Api-Key") != "sk-gateway-secret" || h.Get("Anthropic-Version") != AnthropicVersion {
		t.Errorf("anthropic headers wrong: key=%q version=%q", h.Get("X-Api-Key"), h.Get("Anthropic-Version"))
	}
	if h.Get("Authorization") != "" {
		t.Error("Authorization must not be sent on /messages")
	}
	if strings.Contains(got, "carol@example.com") || strings.Contains(got, "123-45-6789") {
		t.Errorf("system / tool_result PII reached Anthropic: %s", got)
	}
	if strings.Contains(out, "bob@example.org") || !strings.Contains(out, "[REDACTED:EMAIL]") {
		t.Errorf("response PII not redacted: %s", out)
	}
	if e.HitCount < 2 || e.ResponseHits != 1 {
		t.Errorf("audit hits=%d response=%d", e.HitCount, e.ResponseHits)
	}
}

func TestGateway_LocalAnswersAndRefusals(t *testing.T) {
	up := newFakeProvider("application/json", `{}`)
	defer up.srv.Close()
	gw, _ := newGateway(t, ProviderOpenAI, up, DirectionRequest)
	defer gw.Close()

	for _, path := range []string{"/healthz", "/health"} {
		resp, err := http.Get(gw.URL + path)
		if err != nil || resp.StatusCode != 200 {
			t.Errorf("%s: %v %v", path, err, resp)
		}
		_ = resp.Body.Close()
	}
	resp, err := http.Get(gw.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	_ = resp.Body.Close()
	if len(list.Data) != 1 || list.Data[0].ID != "pinned-model" {
		t.Errorf("/v1/models should list only the pinned model: %+v", list)
	}

	if resp, _ := post(t, gw.URL+"/v1/files", `{}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/v1/files must be refused, got %d", resp.StatusCode)
	}
	if resp, _ := post(t, gw.URL+"/v1/chat/completions", `not json alice@example.com`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unparseable body must be refused, got %d", resp.StatusCode)
	}
	if calls, _, _, _ := up.snapshot(); calls != 0 {
		t.Errorf("provider was called %d times; health, models, refused paths and bad bodies must stay local", calls)
	}
}

func TestPlainProxyUnchanged(t *testing.T) {
	// Without a provider the proxy keeps pre-Day-28 behaviour: the caller's
	// auth passes through and the model isn't pinned.
	up := newFakeProvider("application/json", `{}`)
	defer up.srv.Close()
	p, _ := NewProxy(Config{Upstream: up.srv.URL + "/v1", Model: "ignored-without-provider"})
	srv := httptest.NewServer(p)
	defer srv.Close()
	post(t, srv.URL+"/v1/chat/completions", `{"model":"m","messages":[]}`, map[string]string{"Authorization": "Bearer mine"})
	_, _, h, got := up.snapshot()
	if h.Get("Authorization") != "Bearer mine" || strings.Contains(got, "ignored-without-provider") {
		t.Errorf("plain proxy changed behaviour: auth=%q body=%s", h.Get("Authorization"), got)
	}
}

func TestGateway_AnthropicStreamRedactsAcrossChunks(t *testing.T) {
	ev := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	delta := func(text string) string {
		b, _ := json.Marshal(map[string]interface{}{"type": "content_block_delta", "index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": text}})
		return ev("content_block_delta", string(b))
	}
	sse := ev("message_start", `{"type":"message_start","message":{"id":"m1"}}`) +
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		delta("Write to dave@exam") + delta("ple.com today.") +
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		ev("message_stop", `{"type":"message_stop"}`)
	up := newFakeProvider("text/event-stream", sse)
	defer up.srv.Close()
	gw, _ := newGateway(t, ProviderAnthropic, up, DirectionBoth)
	defer gw.Close()

	resp, out := post(t, gw.URL+"/v1/messages", `{"model":"x","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// Re-assemble the way an SDK does: event name + data per block.
	var text strings.Builder
	var events []string
	sc := bufio.NewScanner(strings.NewReader(out))
	name := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var d map[string]interface{}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d); err != nil {
				t.Fatalf("bad data line %q", line)
			}
			if d["type"] != name {
				t.Errorf("event name %q carries data of type %v: stream structure broken", name, d["type"])
			}
			events = append(events, name)
			if dl, ok := d["delta"].(map[string]interface{}); ok {
				s, _ := dl["text"].(string)
				text.WriteString(s)
			}
		}
	}
	if got := text.String(); got != "Write to [REDACTED:EMAIL] today." {
		t.Errorf("streamed text = %q", got)
	}
	if events[len(events)-1] != "message_stop" || events[len(events)-2] != "content_block_stop" {
		t.Errorf("event order broken: %v", events)
	}
}

func TestProxyHardening(t *testing.T) {
	up := newFakeProvider("application/json", `{"choices":[{"message":{"content":"ok"}}]}`)
	defer up.srv.Close()
	audit := newCollectingAudit()
	p, err := NewProxy(Config{Upstream: up.srv.URL + "/v1", Audit: audit, MaxBodyBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	// 1. Bodies over the cap are refused before anything is buffered/forwarded.
	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 2048) + `"}]}`
	if resp, _ := post(t, srv.URL+"/v1/chat/completions", big, nil); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: got %d, want 413", resp.StatusCode)
	}
	if calls, _, _, _ := up.snapshot(); calls != 0 {
		t.Error("oversized body reached the upstream")
	}

	// 2. Audit masks matches by default; 3. big integers survive redaction.
	body := `{"seed":12345678901234567891,"messages":[{"role":"user","content":"mail alice@example.com"}]}`
	post(t, srv.URL+"/v1/chat/completions", body, nil)
	e := audit.next(t)
	if len(e.Matches) != 1 || strings.Contains(e.Matches[0], "alice@example.com") || e.Matches[0] != "al…om (17)" {
		t.Errorf("audit matches should be masked, got %v", e.Matches)
	}
	if _, _, _, got := up.snapshot(); !strings.Contains(got, "12345678901234567891") {
		t.Errorf("seed lost precision in the re-marshalled body: %s", got)
	}
}

func TestAuditMatchesModes(t *testing.T) {
	hits := []Hit{{Label: "EMAIL", Match: "alice@example.com"}, {Label: "PIN", Match: "1234"}}
	for mode, want := range map[string][]string{
		"":     {"al…om (17)", "… (4)"},
		"raw":  {"alice@example.com", "1234"},
		"none": nil,
	} {
		p := &Proxy{cfg: Config{AuditMatches: mode}}
		got := p.auditMatches(hits)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("mode %q: got %v, want %v", mode, got, want)
		}
	}
}

func TestGatewayStripsProviderAccountHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Openai-Organization", "org-secret")
		w.Header().Set("Set-Cookie", "__cf_bm=x")
		w.Header().Set("X-Request-Id", "keep-me")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer up.Close()
	p, _ := NewProxy(Config{Upstream: up.URL + "/v1", Provider: ProviderOpenAI, APIKey: "k"})
	gw := httptest.NewServer(p)
	defer gw.Close()
	resp, _ := post(t, gw.URL+"/v1/chat/completions", `{"messages":[]}`, nil)
	if resp.Header.Get("Openai-Organization") != "" || resp.Header.Get("Set-Cookie") != "" {
		t.Error("provider account headers leaked to the caller")
	}
	if resp.Header.Get("X-Request-Id") != "keep-me" {
		t.Error("ordinary headers should pass through")
	}
}
