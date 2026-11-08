// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// The audit log records the decision, never the payload. If it copied
// prompts, matched values or attachments, the log would become a second
// place holding the very data the proxy protects. (Suggested by an early
// user reviewing the attachment policy.)
func TestAudit_RecordsDecisionNotPayload(t *testing.T) {
	const (
		secretNote = "customer Priya asked about her overdue loan"
		email      = "priya.k@example.com"
		key        = "AKIAIOSFODNN7EXAMPLE"
	)
	imageData := pngURI[strings.Index(pngURI, ",")+1:]

	cases := []struct {
		name   string
		cfg    Config
		body   string
		decide func(t *testing.T, e AuditEntry)
	}{
		{
			name: "redacted text",
			body: `{"messages":[{"role":"user","content":"` + secretNote + `, mail ` + email + `, key ` + key + `"}]}`,
			decide: func(t *testing.T, e AuditEntry) {
				if e.Action != ActionRedact || !contains(e.Labels, "EMAIL") || !contains(e.Labels, "AWS_ACCESS_KEY") || e.HitCount < 2 {
					t.Errorf("decision missing: %+v", e)
				}
			},
		},
		{
			name: "image forwarded (audit)",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"` + secretNote + `"},{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`,
			decide: func(t *testing.T, e AuditEntry) {
				if len(e.Uninspected) != 1 || e.Uninspected[0] != KindImage || e.Action == ActionBlock {
					t.Errorf("decision missing: %+v", e)
				}
			},
		},
		{
			name: "image blocked",
			cfg:  Config{Attachments: AttachmentsBlock},
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"` + secretNote + ` ` + email + `"},{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`,
			decide: func(t *testing.T, e AuditEntry) {
				if e.Action != ActionBlock || len(e.Uninspected) != 1 {
					t.Errorf("decision missing: %+v", e)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newRecordingUpstream()
			defer up.close()
			audit := newCollectingAudit()
			cfg := c.cfg
			cfg.Upstream, cfg.Audit = up.srv.URL+"/v1", audit
			p, err := NewProxy(cfg)
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(p)
			defer srv.Close()
			resp := postChat(t, srv.URL, c.body)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			e := audit.next(t)
			c.decide(t, e)
			line, _ := json.Marshal(e)
			for _, leak := range []string{secretNote, "Priya", email, key, imageData, "image_url"} {
				if strings.Contains(string(line), leak) {
					t.Errorf("audit record contains %q:\n%s", leak, line)
				}
			}
		})
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
