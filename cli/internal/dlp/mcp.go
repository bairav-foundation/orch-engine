// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

// MCP mode: the proxy in front of an MCP server (a Skill).
//
// Model traffic is only half of what an agent sends out. Tool calls carry
// whatever the model decided to pass (often text it read from the user or
// a document), and tool results flow straight back into the model's
// context. MCPProxy inspects both:
//
//	agent ──▶ MCPProxy ──▶ MCP server
//	          request:  every string in each JSON-RPC message
//	                    (tools/call arguments, prompts/get arguments, ...)
//	          response: every string in each JSON-RPC result, whether it
//	                    comes back as a JSON body or as SSE events
//
// It speaks both MCP HTTP transports without knowing which one the server
// uses: POST bodies are JSON-RPC (one message or a batch), and replies are
// either application/json or text/event-stream. Each SSE event is a whole
// JSON-RPC message, so events are redacted one at a time with no hold-back.
//
// Unlike the model proxy there is no path allow-list: every body is
// inspected, whatever the path, and a body that isn't JSON is refused.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MCPBlockedCode is the JSON-RPC error code for a message DLP refused.
// (-32000 to -32099 is reserved for implementation-defined server errors.)
const MCPBlockedCode = -32001

// MCPProxy inspects MCP (JSON-RPC over HTTP / SSE) traffic.
type MCPProxy struct {
	p     *Proxy
	opt   MCPOptions
	tools *toolGuard // nil when tool pinning is off
}

// MCPOptions are the MCP-only settings.
type MCPOptions struct {
	// UpstreamHealth, when set, is a path on the upstream fetched on
	// /healthz, so the Pod is only Ready while the MCP server is.
	UpstreamHealth string
	// UpstreamHeaders are set on every upstream request: the credential
	// for a remote MCP server (Authorization: Bearer ...). When set, the
	// caller's own Authorization / X-Api-Key are dropped, so agents never
	// hold the remote server's token and can't swap in their own.
	UpstreamHeaders http.Header
	// ToolPin is the approved tool list (name -> fingerprint). Nil = trust
	// each tool's first fingerprint and block later changes.
	ToolPin ToolPin
	// ToolPinOff turns tool pinning off entirely.
	ToolPinOff bool
	// SkillName is used in the "bairav skill approve <name>" hint.
	SkillName string
}

// NewMCPProxy builds an MCP proxy. cfg.Upstream is the MCP server's base
// URL: http://127.0.0.1:3000 for a server in the same Pod, or a remote
// one like https://mcp.example.com/mcp. Direction defaults to both: tool
// arguments out, tool results back.
func NewMCPProxy(cfg Config, opt MCPOptions) (*MCPProxy, error) {
	if cfg.Direction == "" {
		cfg.Direction = DirectionBoth
	}
	if cfg.Provider != "" {
		return nil, fmt.Errorf("dlp: MCP mode can't be combined with a provider gateway")
	}
	p, err := NewProxy(cfg)
	if err != nil {
		return nil, err
	}
	m := &MCPProxy{p: p, opt: opt}
	if !opt.ToolPinOff {
		m.tools = newToolGuard(opt.ToolPin, opt.SkillName)
	}
	return m, nil
}

// AuditFailures is the number of audit records that could not be written.
func (m *MCPProxy) AuditFailures() int64 { return m.p.AuditFailures() }

// ServeHTTP implements http.Handler.
func (m *MCPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := m.p
	started := time.Now()
	if r.URL.Path == "/healthz" {
		m.serveHealth(w, r)
		return
	}
	if !p.authorized(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized",
			"bairav: missing or invalid token (Authorization: Bearer <token>)")
		return
	}
	if p.cfg.AuditRequired && p.auditFailures.Load() > 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "audit_unavailable",
			"bairav: audit log is failing and audit is required; refusing requests")
		return
	}

	limit := p.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "dlp_body_too_large",
				fmt.Sprintf("request body over %d bytes", limit))
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	_ = r.Body.Close()

	entry := AuditEntry{Timestamp: started.UTC(), Method: r.Method, Path: r.URL.Path, Direction: p.direction, Action: p.cfg.Action}
	var msg interface{}
	hasBody := len(bytes.TrimSpace(body)) > 0
	if hasBody {
		if err := decodeJSON(body, &msg); err != nil {
			writeJSONError(w, http.StatusBadRequest, "dlp_unparseable",
				"bairav DLP: MCP message is not JSON, so it can't be inspected; not forwarded")
			return
		}
		if _, ok := msg.(map[string]interface{}); !ok {
			if _, ok := msg.([]interface{}); !ok {
				writeJSONError(w, http.StatusBadRequest, "dlp_unparseable",
					"bairav DLP: MCP message must be a JSON-RPC object or batch; not forwarded")
				return
			}
		}
	}

	if hasBody && m.tools != nil {
		if problems := m.tools.callProblems(msg); len(problems) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(mcpErrorReply(msg, MCPToolsChangedCode, m.tools.hint(problems)))
			entry.Event, entry.ToolProblems = "mcp_tools_blocked", problems
			entry.LatencyMS = time.Since(started).Milliseconds()
			p.writeAudit(entry)
			return
		}
	}

	var hits []Hit
	if hasBody && p.direction.inspectsRequest() {
		changed := false
		msg = p.walk(msg, false, &hits, &changed)
		if changed {
			if out, err := json.Marshal(msg); err == nil {
				body = out
			}
		}
	}
	overall := overallAction(hits, p.cfg.Action)
	entry.Labels, entry.HitCount, entry.Action = Labels(hits), len(hits), overall
	entry.LabelActions, entry.Matches = labelActionMap(hits, p.cfg.Action), p.auditMatches(hits)
	if overall == ActionBlock {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(mcpBlockReply(msg, "Blocked by BAIRAVS DLP — detected: "+strings.Join(entry.Labels, ", ")))
		entry.LatencyMS = time.Since(started).Milliseconds()
		p.writeAudit(entry)
		return
	}
	m.forward(w, r, body, &entry, started, hasBody || len(hits) > 0)
}

// mcpBlockReply builds the JSON-RPC error(s) for a blocked message: one per
// request that carries an id, so the client can match them up.
func mcpBlockReply(msg interface{}, text string) interface{} {
	return mcpErrorReply(msg, MCPBlockedCode, text)
}

// mcpErrorReply is mcpBlockReply with any error code.
func mcpErrorReply(msg interface{}, code int, text string) interface{} {
	errFor := func(id interface{}) map[string]interface{} {
		return map[string]interface{}{"jsonrpc": "2.0", "id": id,
			"error": map[string]interface{}{"code": code, "message": text}}
	}
	if batch, ok := msg.([]interface{}); ok {
		out := []interface{}{}
		for _, b := range batch {
			if o, ok := b.(map[string]interface{}); ok && o["id"] != nil {
				out = append(out, errFor(o["id"]))
			}
		}
		return out
	}
	var id interface{}
	if o, ok := msg.(map[string]interface{}); ok {
		id = o["id"]
	}
	return errFor(id)
}

func (m *MCPProxy) forward(w http.ResponseWriter, r *http.Request, body []byte, entry *AuditEntry, started time.Time, audit bool) {
	p := m.p
	target := *p.upstream
	target.Path = singleJoin(target.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), rd)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "dlp_upstream", "build upstream request: "+err.Error())
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("Accept-Encoding") // keep replies readable for inspection
	if len(m.opt.UpstreamHeaders) > 0 {
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
		for k, vs := range m.opt.UpstreamHeaders {
			req.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
		}
	}
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Length")

	resp, err := p.client.Do(req)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "dlp_upstream", "upstream: "+err.Error())
		entry.LatencyMS = time.Since(started).Milliseconds()
		p.writeAudit(*entry)
		return
	}
	defer resp.Body.Close()
	entry.UpstreamCode = resp.StatusCode
	ct := strings.ToLower(resp.Header.Get("Content-Type"))

	switch {
	case (p.direction.inspectsResponse() || m.tools != nil) && strings.Contains(ct, "text/event-stream"):
		// The request is logged now; each SSE event with hits gets its own
		// entry as it passes (a GET stream can stay open for hours).
		if audit {
			entry.LatencyMS = time.Since(started).Milliseconds()
			p.writeAudit(*entry)
		}
		m.forwardSSE(w, r, resp)
		return
	case (p.direction.inspectsResponse() || m.tools != nil) && strings.Contains(ct, "json"):
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		if err != nil || len(raw) > maxResponseBytes {
			// Can't inspect it, so don't pass it on.
			writeJSONError(w, http.StatusBadGateway, "dlp_response_uninspectable",
				"bairav DLP: MCP reply could not be read in full for inspection")
			entry.LatencyMS = time.Since(started).Milliseconds()
			p.writeAudit(*entry)
			return
		}
		raw = m.guardReply(r, raw)
		out, rh := raw, []Hit(nil)
		if p.direction.inspectsResponse() {
			out, rh = m.inspectReply(raw)
		}
		entry.ResponseHits, entry.ResponseLabels = len(rh), Labels(rh)
		copyHeaders(w.Header(), resp.Header)
		w.Header().Set("Content-Length", fmt.Sprint(len(out)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out)
	default:
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
	if audit || entry.ResponseHits > 0 {
		entry.LatencyMS = time.Since(started).Milliseconds()
		p.writeAudit(*entry)
	}
}

// guardReply runs tool pinning over a JSON-RPC reply (or batch): every
// tools/list result is logged with its fingerprints, and one with problems
// is replaced by a JSON-RPC error. Anything else passes unchanged.
func (m *MCPProxy) guardReply(r *http.Request, raw []byte) []byte {
	if m.tools == nil {
		return raw
	}
	var v interface{}
	if err := decodeJSON(raw, &v); err != nil {
		return raw
	}
	msgs, isBatch := v.([]interface{})
	if !isBatch {
		msgs = []interface{}{v}
	}
	changed := false
	for i, one := range msgs {
		o, ok := one.(map[string]interface{})
		if !ok {
			continue
		}
		repl, prints, problems := m.tools.guardMessage(o)
		if prints == nil && problems == nil {
			continue
		}
		e := AuditEntry{Timestamp: time.Now().UTC(), Method: r.Method, Path: r.URL.Path,
			Action: m.p.cfg.Action, Event: "mcp_tools", Tools: prints}
		if len(problems) > 0 {
			e.Event, e.ToolProblems = "mcp_tools_blocked", problems
		}
		m.p.writeAudit(e)
		if repl != nil {
			msgs[i] = repl
			changed = true
		}
	}
	if !changed {
		return raw
	}
	var out interface{} = msgs
	if !isBatch {
		out = msgs[0]
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}

// inspectReply redacts every string in a JSON-RPC reply (or batch). Not
// JSON means not MCP; it passes unchanged (an HTML error page, say).
func (m *MCPProxy) inspectReply(raw []byte) ([]byte, []Hit) {
	var v interface{}
	if err := decodeJSON(raw, &v); err != nil {
		return raw, nil
	}
	var hits []Hit
	changed := false
	v = m.p.walk(v, true, &hits, &changed)
	if !changed {
		return raw, hits
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw, hits
	}
	return out, hits
}

// forwardSSE copies an event stream event by event, redacting the
// JSON-RPC message in each one.
func (m *MCPProxy) forwardSSE(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	var event []string
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				if _, werr := io.WriteString(w, m.rewriteEvent(r, event)+"\n"); werr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
				event = event[:0]
			} else {
				event = append(event, trimmed)
			}
		}
		if err != nil {
			if len(event) > 0 {
				_, _ = io.WriteString(w, m.rewriteEvent(r, event)+"\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
	}
}

// rewriteEvent returns one SSE event (its lines, each ending "\n").
func (m *MCPProxy) rewriteEvent(r *http.Request, lines []string) string {
	var name string
	var data []string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(l, "event:"))
		case strings.HasPrefix(l, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " "))
		}
	}
	if len(data) == 0 {
		return joinLines(lines)
	}
	payload := strings.Join(data, "\n")
	var newData string
	switch name {
	case "endpoint":
		// Legacy SSE transport: where the client should POST. An absolute
		// URL pointing at the upstream would send the client around the
		// proxy, so keep only the path.
		newData = m.relativeEndpoint(payload)
	default:
		guarded := m.guardReply(r, []byte(payload))
		out, hits := guarded, []Hit(nil)
		if m.p.direction.inspectsResponse() {
			out, hits = m.inspectReply(guarded)
		}
		if len(hits) > 0 {
			p := m.p
			p.writeAudit(AuditEntry{
				Timestamp: time.Now().UTC(), Method: "SSE", Path: r.URL.Path, Direction: DirectionResponse,
				Action: p.cfg.Action, ResponseHits: len(hits), ResponseLabels: Labels(hits),
				LabelActions: labelActionMap(hits, p.cfg.Action), Matches: p.auditMatches(hits),
			})
		}
		newData = string(out)
	}
	if newData == payload {
		return joinLines(lines)
	}
	var b strings.Builder
	for _, l := range lines {
		if !strings.HasPrefix(l, "data:") {
			b.WriteString(l + "\n")
		}
	}
	for _, d := range strings.Split(newData, "\n") {
		b.WriteString("data: " + d + "\n")
	}
	return b.String()
}

func (m *MCPProxy) relativeEndpoint(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" || !strings.EqualFold(u.Host, m.p.upstream.Host) {
		return s
	}
	out := u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// serveHealth answers /healthz: the proxy's own health, and the MCP
// server's when an upstream health path is set.
func (m *MCPProxy) serveHealth(w http.ResponseWriter, r *http.Request) {
	p := m.p
	auditFailing := p.cfg.AuditRequired && p.auditFailures.Load() > 0
	if m.opt.UpstreamHealth != "" && !auditFailing {
		u := *p.upstream
		u.Path = singleJoin(u.Path, m.opt.UpstreamHealth)
		u.RawQuery = ""
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
		if err == nil {
			hc := &http.Client{Timeout: 3 * time.Second}
			var resp *http.Response
			if resp, err = hc.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
				if resp.StatusCode >= 400 {
					err = fmt.Errorf("upstream health: HTTP %d", resp.StatusCode)
				}
			}
		}
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "upstream_unhealthy", "error": err.Error()})
			return
		}
	}
	p.serveHealth(w)
}
