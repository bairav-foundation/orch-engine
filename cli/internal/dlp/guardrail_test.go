// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type memAudit struct{ entries []AuditEntry }

func (m *memAudit) Write(e AuditEntry) error { m.entries = append(m.entries, e); return nil }

func callGuardrail(t *testing.T, g http.Handler, body string, hdr map[string]string) (int, guardrailResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, LiteLLMGuardrailPath, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	var out guardrailResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("bad JSON %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

func TestGuardrailVerdicts(t *testing.T) {
	audit := &memAudit{}
	g := NewGuardrail(Config{Audit: audit})

	// Nothing sensitive: NONE.
	code, r := callGuardrail(t, g, `{"texts":["hello there"],"input_type":"request"}`, nil)
	if code != 200 || r.Action != guardrailNone || r.Texts != nil {
		t.Fatalf("clean text: %d %+v", code, r)
	}

	// Redact: texts come back rewritten one to one, untouched entries kept.
	code, r = callGuardrail(t, g, `{"texts":["mail jane@corp.com please","no pii here"],"input_type":"request"}`, nil)
	if code != 200 || r.Action != guardrailIntervened || len(r.Texts) != 2 ||
		r.Texts[0] != "mail [REDACTED:EMAIL] please" || r.Texts[1] != "no pii here" {
		t.Fatalf("redact: %d %+v", code, r)
	}

	// The same folding as the proxy: full-width digits are still a card.
	code, r = callGuardrail(t, g, `{"texts":["card ４１１１ １１１１ １１１１ １１１１"]}`, nil)
	if r.Action != guardrailIntervened || !strings.Contains(r.Texts[0], "[REDACTED:CREDIT_CARD]") {
		t.Fatalf("folded card: %d %+v", code, r)
	}

	// Sensitive data in tool-call arguments can't be rewritten: block.
	code, r = callGuardrail(t, g, `{"texts":[],"tool_calls":[{"id":"c1","type":"function","function":{"name":"send","arguments":"{\"to\":\"jane@corp.com\"}"}}]}`, nil)
	if r.Action != guardrailBlocked || !strings.Contains(r.BlockedReason, "tool call") {
		t.Fatalf("tool call: %d %+v", code, r)
	}

	if len(audit.entries) != 4 || audit.entries[1].Labels[0] != "EMAIL" {
		t.Fatalf("audit entries: %+v", audit.entries)
	}
	for _, e := range audit.entries {
		for _, m := range e.Matches {
			if strings.Contains(m, "jane@corp.com") {
				t.Fatalf("raw match in audit by default: %q", m)
			}
		}
	}
}

func TestGuardrailBlockAction(t *testing.T) {
	g := NewGuardrail(Config{Action: ActionBlock})
	_, r := callGuardrail(t, g, `{"texts":["my ssn is 123-45-6789"],"input_type":"response"}`, nil)
	if r.Action != guardrailBlocked || !strings.Contains(r.BlockedReason, "SSN") {
		t.Fatalf("block: %+v", r)
	}
}

func TestGuardrailAuthAndRoutes(t *testing.T) {
	g := NewGuardrail(Config{ClientTokens: []string{"0123456789abcdef"}})
	if code, _ := callGuardrail(t, g, `{"texts":["x"]}`, nil); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := callGuardrail(t, g, `{"texts":["x"]}`, map[string]string{"Authorization": "Bearer 0123456789abcdef"}); code != 200 {
		t.Errorf("good token: %d", code)
	}

	// Anything but the guardrail endpoint is 404; nothing is forwarded.
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("other path: %d", rec.Code)
	}
	// Health stays open for probes.
	rec = httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: %d", rec.Code)
	}
	// Unparseable bodies fail closed (LiteLLM blocks on guardrail errors by default).
	g2 := NewGuardrail(Config{})
	if code, _ := callGuardrail(t, g2, `not json`, nil); code != http.StatusBadRequest {
		t.Errorf("bad json: %d", code)
	}
}
