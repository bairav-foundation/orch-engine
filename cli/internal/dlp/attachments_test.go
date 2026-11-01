// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const pngURI = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func TestAttachmentKinds(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
	}{
		"text only": {`{"messages":[{"role":"user","content":"hello"}]}`, []string{}},
		"openai image data": {`{"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},` +
			`{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`, []string{"image"}},
		"openai image url":                {`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/id.png"}}]}]}`, []string{"image"}},
		"openai pdf file":                 {`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"statement.pdf","file_data":"data:application/pdf;base64,JVBERi0="}}]}]}`, []string{"pdf"}},
		"openai audio":                    {`{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}]}]}`, []string{"audio"}},
		"responses image":                 {`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"` + pngURI + `"}]}]}`, []string{"image"}},
		"anthropic image":                 {`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"/9j/4AAQ"}}]}]}`, []string{"image"}},
		"anthropic pdf":                   {`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}]}]}`, []string{"pdf"}},
		"anthropic text document is text": {`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"plain words"}}]}]}`, []string{}},
		"bare data uri anywhere":          {`{"metadata":{"attachment":"` + pngURI + `"}}`, []string{"image"}},
		"several kinds, sorted":           {`{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"x"}},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`, []string{"audio", "image"}},
	}
	for name, c := range cases {
		var v interface{}
		if err := json.Unmarshal([]byte(c.body), &v); err != nil {
			t.Fatalf("%s: bad test body: %v", name, err)
		}
		if got := attachmentKinds(v); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

func TestParseAttachmentMode(t *testing.T) {
	for in, want := range map[string]AttachmentMode{"": AttachmentsAudit, "audit": AttachmentsAudit, " Block ": AttachmentsBlock} {
		if got, ok := ParseAttachmentMode(in); !ok || got != want {
			t.Errorf("%q: got %q %v", in, got, ok)
		}
	}
	if _, ok := ParseAttachmentMode("allow"); ok {
		t.Error("unknown mode accepted")
	}
	if _, err := NewProxy(Config{Upstream: "http://x/v1", Attachments: "maybe"}); err == nil {
		t.Error("NewProxy accepted a bad attachments mode")
	}
}

const imageChat = `{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":"read this card"},{"type":"image_url","image_url":{"url":"` + pngURI + `"}}]}]}`

func postChat(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestProxy_AttachmentsAuditForwardsAndFlags(t *testing.T) {
	up := newRecordingUpstream()
	defer up.close()
	audit := newCollectingAudit()
	p, err := NewProxy(Config{Upstream: up.srv.URL + "/v1", Audit: audit})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp := postChat(t, srv.URL, imageChat)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get(UninspectedHeader) != "image" {
		t.Fatalf("status %d, %s %q", resp.StatusCode, UninspectedHeader, resp.Header.Get(UninspectedHeader))
	}
	e := audit.next(t)
	if !reflect.DeepEqual(e.Uninspected, []string{"image"}) || e.Action == ActionBlock {
		t.Fatalf("audit: uninspected %v action %q", e.Uninspected, e.Action)
	}
	up.mu.Lock()
	forwarded := strings.Contains(string(up.gotBody), "image_url")
	up.mu.Unlock()
	if !forwarded {
		t.Fatal("image request was not forwarded in audit mode")
	}

	// A text-only request carries no header and no uninspected list.
	resp = postChat(t, srv.URL, `{"messages":[{"role":"user","content":"hi"}]}`)
	resp.Body.Close()
	if h := resp.Header.Get(UninspectedHeader); h != "" {
		t.Fatalf("text request flagged as %q", h)
	}
	if e := audit.next(t); len(e.Uninspected) != 0 {
		t.Fatalf("text request audited as uninspected: %v", e.Uninspected)
	}
}

func TestProxy_AttachmentsBlockRefuses(t *testing.T) {
	up := newRecordingUpstream()
	defer up.close()
	audit := newCollectingAudit()
	p, err := NewProxy(Config{Upstream: up.srv.URL + "/v1", Audit: audit, Attachments: AttachmentsBlock})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp := postChat(t, srv.URL, imageChat)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "dlp_attachment_blocked") || !strings.Contains(string(b), "image") {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	e := audit.next(t)
	if e.Action != ActionBlock || !reflect.DeepEqual(e.Uninspected, []string{"image"}) {
		t.Fatalf("audit: %+v", e)
	}
	up.mu.Lock()
	got := up.gotBody
	up.mu.Unlock()
	if got != nil {
		t.Fatalf("blocked request reached the upstream: %s", got)
	}

	// Text still flows under block.
	resp = postChat(t, srv.URL, `{"messages":[{"role":"user","content":"hi"}]}`)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("text request refused: %d", resp.StatusCode)
	}
}

func TestGuardrail_Images(t *testing.T) {
	audit := &memAudit{}
	g := NewGuardrail(Config{Audit: audit})
	_, r := callGuardrail(t, g, `{"texts":["what is in this picture"],"images":["`+pngURI+`"],"input_type":"request"}`, nil)
	if r.Action != guardrailNone || len(audit.entries) != 1 || !reflect.DeepEqual(audit.entries[0].Uninspected, []string{"image"}) {
		t.Fatalf("audit mode: %+v %+v", r, audit.entries)
	}

	audit = &memAudit{}
	g = NewGuardrail(Config{Audit: audit, Attachments: AttachmentsBlock})
	_, r = callGuardrail(t, g, `{"texts":["hi"],"images":["`+pngURI+`"],"input_type":"request"}`, nil)
	if r.Action != guardrailBlocked || !strings.Contains(r.BlockedReason, "images") || audit.entries[0].Action != ActionBlock {
		t.Fatalf("block mode: %+v %+v", r, audit.entries)
	}
}
