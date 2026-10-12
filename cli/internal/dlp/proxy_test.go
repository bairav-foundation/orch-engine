// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectingAudit is a channel-backed AuditWriter used by the proxy tests.
//
// Why not a bytes.Buffer: the proxy writes the audit entry AFTER the HTTP
// response is sent, on the server goroutine. The test goroutine — which
// calls http.Client.Do and then reads the audit — has no guarantee that
// the server-side audit write has happened by the time Do returns. A
// bytes.Buffer accessed from both goroutines races (go test -race catches
// this). A buffered channel lets the test *wait* for the entry.
type collectingAudit struct {
	entries chan AuditEntry
}

func newCollectingAudit() *collectingAudit {
	return &collectingAudit{entries: make(chan AuditEntry, 16)}
}

func (c *collectingAudit) Write(e AuditEntry) error {
	c.entries <- e
	return nil
}

// next blocks up to 2s waiting for the next entry. Fails the test on timeout.
func (c *collectingAudit) next(t *testing.T) AuditEntry {
	t.Helper()
	select {
	case e := <-c.entries:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for audit entry")
		return AuditEntry{}
	}
}

// recordingUpstream is a fake OpenAI-compatible model server. It captures
// the request body it received so the test can assert what the proxy
// actually forwarded.
type recordingUpstream struct {
	srv      *httptest.Server
	gotBody  []byte
	gotPath  string
	respCode int
	respBody string
	mu       sync.Mutex
}

func newRecordingUpstream() *recordingUpstream {
	u := &recordingUpstream{respCode: 200, respBody: `{"id":"ok"}`}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.gotBody = body
		u.gotPath = r.URL.Path
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.respCode)
		_, _ = w.Write([]byte(u.respBody))
	}))
	return u
}

func (u *recordingUpstream) close() { u.srv.Close() }

func TestProxy_RedactsChatCompletionBeforeForwarding(t *testing.T) {
	up := newRecordingUpstream()
	defer up.close()

	audit := newCollectingAudit()
	p, err := NewProxy(Config{
		Upstream: up.srv.URL + "/v1",
		Action:   ActionRedact,
		Audit:    audit,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	body := `{"model":"x","messages":[{"role":"user","content":"my email is alice@example.com and SSN 123-45-6789"}]}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Wait for the audit entry — this also acts as a synchronization point
	// so the assertions on up.gotBody below don't race with the server's
	// forward goroutine.
	entry := audit.next(t)

	// The upstream must have seen redacted content, NOT the raw PII.
	up.mu.Lock()
	gotBody := string(up.gotBody)
	up.mu.Unlock()
	if strings.Contains(gotBody, "alice@example.com") {
		t.Errorf("upstream saw raw email — proxy did not redact: %s", gotBody)
	}
	if strings.Contains(gotBody, "123-45-6789") {
		t.Errorf("upstream saw raw SSN — proxy did not redact: %s", gotBody)
	}
	if !strings.Contains(gotBody, "[REDACTED:EMAIL]") {
		t.Errorf("expected EMAIL placeholder in upstream body: %s", gotBody)
	}
	if !strings.Contains(gotBody, "[REDACTED:SSN]") {
		t.Errorf("expected SSN placeholder in upstream body: %s", gotBody)
	}

	if entry.HitCount != 2 {
		t.Errorf("audit HitCount = %d, want 2", entry.HitCount)
	}
	if entry.Action != ActionRedact {
		t.Errorf("audit Action = %q, want redact", entry.Action)
	}
}

func TestProxy_BlocksWhenActionBlock(t *testing.T) {
	up := newRecordingUpstream()
	defer up.close()

	audit := newCollectingAudit()
	p, _ := NewProxy(Config{
		Upstream: up.srv.URL + "/v1",
		Action:   ActionBlock,
		Audit:    audit,
	})
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	body := `{"model":"x","messages":[{"role":"user","content":"SSN 123-45-6789"}]}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	// Wait for audit entry so we can safely read up.gotBody.
	entry := audit.next(t)
	if entry.Action != ActionBlock {
		t.Errorf("audit Action = %q, want block", entry.Action)
	}

	up.mu.Lock()
	sawUpstream := up.gotBody != nil
	up.mu.Unlock()
	if sawUpstream {
		t.Errorf("upstream should NOT have been called when blocked")
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	var errResp map[string]interface{}
	_ = json.Unmarshal(bodyBytes, &errResp)
	if errResp["error"] == nil {
		t.Errorf("expected error envelope in 403 response, got %s", bodyBytes)
	}
}

func TestProxy_AuditOnlyForwardsUnchanged(t *testing.T) {
	up := newRecordingUpstream()
	defer up.close()

	audit := newCollectingAudit()
	p, _ := NewProxy(Config{
		Upstream: up.srv.URL + "/v1",
		Action:   ActionAuditOnly,
		Audit:    audit,
	})
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	rawContent := "ping alice@example.com please"
	body := `{"model":"x","messages":[{"role":"user","content":"` + rawContent + `"}]}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()

	entry := audit.next(t)
	if entry.HitCount != 1 {
		t.Errorf("audit HitCount = %d, want 1", entry.HitCount)
	}
	if entry.Action != ActionAuditOnly {
		t.Errorf("audit Action = %q, want audit_only", entry.Action)
	}

	up.mu.Lock()
	gotBody := string(up.gotBody)
	up.mu.Unlock()
	if !strings.Contains(gotBody, "alice@example.com") {
		t.Errorf("audit_only must forward unmodified body, got: %s", gotBody)
	}
}

func TestProxy_PassesThroughUnknownPaths(t *testing.T) {
	up := newRecordingUpstream()
	defer up.close()

	p, _ := NewProxy(Config{
		Upstream: up.srv.URL + "/v1",
		Action:   ActionRedact,
	})
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	// /models is read-only metadata; we never inspect, but still proxy.
	req, _ := http.NewRequest("GET", proxy.URL+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	_ = resp.Body.Close()
	up.mu.Lock()
	gotPath := up.gotPath
	up.mu.Unlock()
	if gotPath != "/v1/models" {
		t.Errorf("upstream path = %q, want /v1/models", gotPath)
	}
}

func TestProxy_RejectsBadUpstream(t *testing.T) {
	if _, err := NewProxy(Config{Upstream: ""}); err == nil {
		t.Error("expected error for empty upstream")
	}
	if _, err := NewProxy(Config{Upstream: "not a url"}); err == nil {
		t.Error("expected error for relative upstream")
	}
}

func TestProxy_MixedPolicy_BlockWinsOverRedact(t *testing.T) {
	// Two custom rules: one blocks, one redacts. Global default is redact.
	// A prompt that hits both should be BLOCKED (block wins), and the
	// audit entry should record per-label actions so a SOC reviewer can
	// see which rule actually triggered the block.
	up := newRecordingUpstream()
	defer up.close()

	policy := `
version: v1
rules:
  - label: INTERNAL_TOKEN
    pattern: 'bkey_[A-Za-z0-9]{16,}'
    action: block
  - label: CUST_ID
    pattern: 'CUST-\d{7}'
    action: redact
`
	extraDetectors, _, err := ParsePolicy([]byte(policy), "test.yaml")
	if err != nil {
		t.Fatalf("policy parse: %v", err)
	}
	dets := append(DefaultDetectors(), extraDetectors...)

	audit := newCollectingAudit()
	p, err := NewProxy(Config{
		Upstream:  up.srv.URL + "/v1",
		Action:    ActionRedact, // global default; the block rule must override
		Detectors: dets,
		Audit:     audit,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	body := `{"model":"x","messages":[{"role":"user","content":"customer CUST-4477291 has token bkey_ABCDEFGHIJKLMNOP"}]}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403 (block wins over redact)", resp.StatusCode)
	}

	entry := audit.next(t)
	if entry.Action != ActionBlock {
		t.Errorf("overall Action = %q, want block", entry.Action)
	}
	if entry.LabelActions["INTERNAL_TOKEN"] != ActionBlock {
		t.Errorf("LabelActions[INTERNAL_TOKEN] = %q, want block", entry.LabelActions["INTERNAL_TOKEN"])
	}
	if entry.LabelActions["CUST_ID"] != ActionRedact {
		t.Errorf("LabelActions[CUST_ID] = %q, want redact", entry.LabelActions["CUST_ID"])
	}

	// Upstream must never have been called.
	up.mu.Lock()
	sawUpstream := up.gotBody != nil
	up.mu.Unlock()
	if sawUpstream {
		t.Errorf("upstream was called on a block-effective request")
	}
}

func TestProxy_MixedPolicy_RedactOnlyBlockingHits(t *testing.T) {
	// One custom rule blocks, one redacts; the request contains only a
	// redact-effective hit (built-in EMAIL). Overall action must be Redact,
	// the body must reach upstream with the placeholder in place.
	up := newRecordingUpstream()
	defer up.close()

	policy := `
version: v1
rules:
  - label: INTERNAL_TOKEN
    pattern: 'bkey_[A-Za-z0-9]{16,}'
    action: block
`
	extra, _, err := ParsePolicy([]byte(policy), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dets := append(DefaultDetectors(), extra...)

	audit := newCollectingAudit()
	p, _ := NewProxy(Config{
		Upstream:  up.srv.URL + "/v1",
		Action:    ActionRedact,
		Detectors: dets,
		Audit:     audit,
	})
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	body := `{"model":"x","messages":[{"role":"user","content":"ping alice@example.com please"}]}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()

	entry := audit.next(t)
	if entry.Action != ActionRedact {
		t.Errorf("overall Action = %q, want redact (only EMAIL hit)", entry.Action)
	}

	up.mu.Lock()
	gotBody := string(up.gotBody)
	up.mu.Unlock()
	if !strings.Contains(gotBody, "[REDACTED:EMAIL]") {
		t.Errorf("upstream should see EMAIL placeholder, got: %s", gotBody)
	}
	if strings.Contains(gotBody, "alice@example.com") {
		t.Errorf("raw email leaked to upstream: %s", gotBody)
	}
}

func TestProxy_MixedPolicy_AuditOnlyDoesNotRedact(t *testing.T) {
	// A rule marked audit_only should log the hit but leave the body
	// unchanged, even when the global default is redact.
	up := newRecordingUpstream()
	defer up.close()

	policy := `
version: v1
rules:
  - label: INTERNAL_TICKET
    pattern: 'ENG-\d{4,6}'
    action: audit_only
`
	extra, _, err := ParsePolicy([]byte(policy), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dets := append(DefaultDetectors(), extra...)

	audit := newCollectingAudit()
	p, _ := NewProxy(Config{
		Upstream:  up.srv.URL + "/v1",
		Action:    ActionRedact,
		Detectors: dets,
		Audit:     audit,
	})
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	body := `{"model":"x","messages":[{"role":"user","content":"tracking ENG-12345 progress"}]}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()

	entry := audit.next(t)
	if entry.LabelActions["INTERNAL_TICKET"] != ActionAuditOnly {
		t.Errorf("LabelActions[INTERNAL_TICKET] = %q, want audit_only", entry.LabelActions["INTERNAL_TICKET"])
	}

	up.mu.Lock()
	gotBody := string(up.gotBody)
	up.mu.Unlock()
	if !strings.Contains(gotBody, "ENG-12345") {
		t.Errorf("audit_only must NOT redact; ticket ID should still be in upstream body: %s", gotBody)
	}
}
