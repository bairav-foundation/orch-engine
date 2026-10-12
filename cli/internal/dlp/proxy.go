// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"
)

// Action is what the proxy does when a hit is found.
type Action string

const (
	// ActionRedact replaces each detected span with [REDACTED:LABEL] before
	// forwarding to the upstream. This is the default — preserves UX while
	// keeping the secret out of the model's input.
	ActionRedact Action = "redact"

	// ActionBlock rejects the request with HTTP 403 and a JSON error body.
	// No prompt is forwarded; nothing reaches the model. Use for hard PII
	// like SSN or PAN in regulated environments.
	ActionBlock Action = "block"

	// ActionAuditOnly logs the hit but forwards the unmodified request.
	// Useful for measuring exposure before turning enforcement on.
	ActionAuditOnly Action = "audit_only"
)

// Direction is which side of the Model call the proxy inspects.
//
// Pre-Day-18 the proxy only inspected the request. Day 18 adds
// response-side inspection so a Model that returns PII in its
// completion gets scrubbed before the caller sees it. Both sides use
// the same detector set — the direction only picks which body(s) the
// proxy actually walks.
type Direction string

// Direction values.
const (
	DirectionRequest  Direction = "request"
	DirectionResponse Direction = "response"
	DirectionBoth     Direction = "both"
)

// inspectsRequest reports whether the proxy should scan the request body.
func (d Direction) inspectsRequest() bool {
	return d == "" || d == DirectionRequest || d == DirectionBoth
}

// inspectsResponse reports whether the proxy should scan the response body.
func (d Direction) inspectsResponse() bool {
	return d == DirectionResponse || d == DirectionBoth
}

// Config configures one proxy instance.
type Config struct {
	// Upstream is the base URL of the model endpoint we proxy to,
	// e.g. "http://127.0.0.1:8080/v1". Required.
	Upstream string

	// Detectors is the rule set; nil means DefaultDetectors().
	Detectors []Detector

	// Action is the global enforcement action on any hit. Default redact.
	Action Action

	// Audit, if non-nil, receives one entry per request. The CLI wires this
	// to a JSONL file writer; tests can inject a buffer.
	Audit AuditWriter

	// Direction is which side of the traffic to inspect. Empty = request
	// (pre-Day-18 default). "response" or "both" activates response-side
	// scanning of OpenAI responses: JSON bodies are buffered and
	// rewritten (Day 18); SSE streams on /chat/completions and
	// /completions are redacted incrementally with a hold-back window so
	// spans split across chunks are still caught (Day 19, stream.go).
	Direction Direction

	// Provider turns on gateway mode (Day 28, gateway.go): "openai",
	// "anthropic" or "openai-compatible". Empty = plain DLP proxy in front
	// of a model the caller could otherwise reach (pre-Day-28 behaviour).
	Provider string

	// APIKey is the provider key the gateway injects. Callers' own
	// credentials are always stripped in gateway mode. Never logged.
	APIKey string

	// Model pins the request "model" field (gateway mode). Empty = pass
	// the caller's choice through.
	Model string

	// MaxBodyBytes caps a request body (0 = DefaultMaxBodyBytes). Bigger
	// requests get 413 instead of being buffered into memory.
	MaxBodyBytes int64

	// ClientTokens, when non-empty, are the only credentials accepted from
	// callers ("Authorization: Bearer <token>" or "X-Api-Key: <token>").
	// Requests without one get 401 before anything is read or forwarded.
	// In gateway mode this is what stops any pod that can reach the
	// Service from spending the organisation's provider key.
	ClientTokens []string

	// AuditRequired makes a failed audit write fatal: the proxy reports
	// unhealthy on /healthz and refuses new requests with 503 until it is
	// restarted. For deployments where an unaudited request must not happen.
	AuditRequired bool

	// AuditMatches controls how matched text appears in the audit log:
	// "masked" (default: first/last 2 chars + length), "raw", or "none".
	// Raw puts the very PII the proxy protects into every log pipeline
	// that ingests the audit stream, so it is opt-in.
	AuditMatches string
}

// DefaultMaxBodyBytes bounds request bodies. 16 MiB leaves room for base64
// images in vision prompts; peak memory per request is a few times this
// (raw body, decoded JSON, re-encoded copy), which is why the operator
// gives DLP containers a 512Mi limit.
const DefaultMaxBodyBytes = 16 << 20

// maxResponseBytes bounds a buffered (non-streaming) upstream reply that
// response-side inspection has to hold in memory.
const maxResponseBytes = 64 << 20

// decodeJSON is json.Unmarshal with UseNumber, so numbers we pass through
// untouched (seeds, token ids, logit_bias keys' values) keep full
// precision when the body is re-marshalled after redaction.
func decodeJSON(body []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	return dec.Decode(v)
}

// AuditWriter is anything that can record an audit entry. A
// NewFileAuditWriter helper writes JSONL; tests pass a buffer-backed
// implementation.
type AuditWriter interface {
	Write(entry AuditEntry) error
}

// AuditEntry is one proxy decision, serialised as one line of JSONL.
type AuditEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`

	// Direction is which side(s) inspection ran on for this request:
	// "request", "response", or "both". Blank for pre-Day-18 audit
	// entries — those always meant request-side.
	Direction Direction `json:"direction,omitempty"`

	// ResponseHits is the number of hits the response scanner found
	// (only populated when Direction includes response). Kept as a
	// separate counter from HitCount so SOC dashboards can tell "PII
	// leaving via the caller's prompt" from "PII coming back in the
	// completion" without re-scanning.
	ResponseHits int `json:"response_hits,omitempty"`

	// ResponseLabels lists the labels that fired on the response side.
	ResponseLabels []string `json:"response_labels,omitempty"`

	// Action is the OVERALL action the proxy took on this request. When
	// every hit inherits the global action, this equals --action. When
	// custom policy rules mix actions (e.g. one rule blocks, another
	// redacts), this is the strongest observed action (block > redact >
	// audit_only) — the actual outcome, not the configured default.
	Action Action `json:"action"`

	Labels   []string `json:"labels,omitempty"` // unique detector labels triggered
	HitCount int      `json:"hit_count"`        // total hits across all detectors

	// LabelActions gives the per-label effective action for every label
	// that fired on this request — useful when a mixed policy takes
	// different actions on different labels and the SOC team needs to
	// know which rule tripped which behaviour. Absent when empty.
	LabelActions map[string]Action `json:"label_actions,omitempty"`

	UpstreamCode int   `json:"upstream_code,omitempty"`
	LatencyMS    int64 `json:"latency_ms"`

	// Matches is the verbatim list of matched substrings. Useful for SOC
	// review of false positives. Customer deployments can mask this in the
	// audit pipeline if it's too sensitive to retain.
	Matches []string `json:"matches,omitempty"`
}

// Proxy is the HTTP handler that intercepts OpenAI-compatible model
// requests, applies DLP policy, and forwards to the configured upstream.
type Proxy struct {
	cfg       Config
	upstream  *url.URL
	detectors []Detector
	direction Direction
	client    *http.Client
	// auditFailures counts audit records that could not be written.
	auditFailures atomic.Int64
}

// NewProxy constructs a Proxy from cfg. Returns an error if Upstream is
// missing or unparseable.
func NewProxy(cfg Config) (*Proxy, error) {
	if cfg.Upstream == "" {
		return nil, fmt.Errorf("dlp: Upstream is required")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("dlp: parse upstream: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("dlp: upstream must be an absolute URL (got %q)", cfg.Upstream)
	}
	detectors := cfg.Detectors
	if detectors == nil {
		detectors = DefaultDetectors()
	}
	action := cfg.Action
	if action == "" {
		action = ActionRedact
	}
	cfg.Action = action
	direction := cfg.Direction
	if direction == "" {
		direction = DirectionRequest
	}
	cfg.Direction = direction
	return &Proxy{
		cfg:       cfg,
		upstream:  u,
		detectors: detectors,
		direction: direction,
		client: &http.Client{
			// No timeout — chat completions can stream for minutes. The
			// caller's context cancels via the request, which is what we
			// want for upstream too.
			Timeout: 0,
			// Never follow redirects: Go would re-send the (inspected)
			// body to wherever the upstream points, and a redirect is the
			// classic way to land a request on a route we didn't check.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	// One spelling per route: "/v1/Chat/Completions/" must be inspected
	// exactly like "/v1/chat/completions", and is forwarded as such.
	r.URL.Path, r.URL.RawPath = canonicalPath(r.URL.Path), ""
	if r.URL.Path == "/healthz" || (p.gatewayMode() && r.URL.Path == "/health") {
		p.serveHealth(w)
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
	if p.handleLocal(w, r) {
		return
	}
	// Fail closed on anything carrying a body that we don't know how to
	// inspect (llama-server's native /completion, /infill, /tokenize, ...):
	// otherwise a caller simply picks an uninspected endpoint.
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if !isInferencePath(r.URL.Path) {
			writeJSONError(w, http.StatusNotFound, "path_not_inspected",
				fmt.Sprintf("bairav DLP: %s %s is not an inspected inference path", r.Method, r.URL.Path))
			return
		}
	}

	// Read the body once so we can inspect AND forward it, bounded so a
	// huge or endless body can't exhaust memory.
	limit := p.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "dlp_body_too_large",
				fmt.Sprintf("request body over %d bytes", limit))
			return
		}
		http.Error(w, fmt.Sprintf("dlp proxy: read body: %v", err), http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	// Fail closed: whatever we can't parse, we can't inspect, so it isn't
	// forwarded (gateway and sidecar alike).
	if isInferencePath(r.URL.Path) && len(bytes.TrimSpace(bodyBytes)) > 0 && !validJSONObject(bodyBytes) {
		writeJSONError(w, http.StatusBadRequest, "dlp_unparseable",
			"bairav DLP: request body is not a JSON object, so it can't be inspected; not forwarded")
		return
	}

	// Request-side inspection (skipped entirely when direction=response).
	// inspect returns a body with only the redact-effective spans replaced.
	// block-effective and audit_only-effective spans are NOT redacted in the
	// body (block short-circuits before forwarding; audit_only means "let it
	// through"), but they still appear in hits so they're logged and can
	// escalate the overall decision.
	var (
		rewritten = bodyBytes
		hits      []Hit
	)
	if p.direction.inspectsRequest() {
		rewritten, hits = p.inspect(r.URL.Path, bodyBytes)
	}
	overall := overallAction(hits, p.cfg.Action)

	entry := AuditEntry{
		Timestamp:    started.UTC(),
		Method:       r.Method,
		Path:         r.URL.Path,
		Direction:    p.direction,
		Action:       overall,
		Labels:       Labels(hits),
		HitCount:     len(hits),
		LabelActions: labelActionMap(hits, p.cfg.Action),
		Matches:      p.auditMatches(hits),
	}

	// Two outcomes: block short-circuits with 403; every other overall
	// action (redact, audit_only, or "no hits") forwards. In the no-hit
	// and audit_only cases rewritten == bodyBytes (nothing was redacted).
	if overall == ActionBlock {
		writeJSONError(w, http.StatusForbidden, "dlp_block",
			fmt.Sprintf("Request blocked by BAIRAVS DLP — detected: %s", strings.Join(entry.Labels, ", ")))
		entry.LatencyMS = time.Since(started).Milliseconds()
		p.writeAudit(entry)
		return
	}
	if p.gatewayMode() {
		rewritten = p.pinModel(rewritten)
	}
	p.forward(w, r, rewritten, &entry, started)
}

// effectiveAction returns h's per-rule action if set, else the global fallback.
func effectiveAction(h Hit, global Action) Action {
	if h.Action != "" {
		return h.Action
	}
	return global
}

// overallAction folds every hit's effective action into the single decision
// the proxy will take on this request. Precedence: block > redact > audit_only.
// No hits → redact (a no-op forward — same shape as if all hits were audit_only).
func overallAction(hits []Hit, global Action) Action {
	if len(hits) == 0 {
		return ActionRedact
	}
	seen := map[Action]bool{}
	for _, h := range hits {
		seen[effectiveAction(h, global)] = true
	}
	if seen[ActionBlock] {
		return ActionBlock
	}
	if seen[ActionRedact] {
		return ActionRedact
	}
	return ActionAuditOnly
}

// labelActionMap builds the per-label effective-action map for the audit
// entry. Returns nil (not an empty map) when there are no hits so the JSON
// output omits the field entirely.
func labelActionMap(hits []Hit, global Action) map[string]Action {
	if len(hits) == 0 {
		return nil
	}
	out := map[string]Action{}
	for _, h := range hits {
		out[h.Label] = effectiveAction(h, global)
	}
	return out
}

// inferencePaths are the request paths whose bodies the proxy knows how to
// inspect (OpenAI and Anthropic styles). Matched on the canonical path
// (canonicalPath), as a suffix so any /v1 prefix works.
var inferencePaths = []string{
	"/chat/completions", "/completions", "/embeddings",
	"/messages", "/messages/count_tokens", "/responses",
}

func isInferencePath(path string) bool {
	for _, s := range inferencePaths {
		if strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}

// canonicalPath folds the spellings an HTTP server may treat as the same
// route ("/v1/Chat/Completions/", "/v1//chat/completions") into one, so a
// variant can't skip inspection and then be redirected upstream to the
// real endpoint with the raw body.
func canonicalPath(p string) string {
	c := path.Clean("/" + strings.ToLower(p))
	if c == "." {
		return "/"
	}
	return c
}

// inspect scans and redacts a request body. Every string in the JSON
// document is scanned except a short reviewed list of identifier keys
// (walkSkipKeys): scanning only the fields we know about left new or
// unusual fields (tool descriptions, metadata, Anthropic document blocks,
// Responses prompt variables, ...) as silent bypasses.
func (p *Proxy) inspect(path string, body []byte) (rewritten []byte, hits []Hit) {
	if !isInferencePath(path) {
		return body, nil
	}
	return p.walkBody(body, false)
}

// forward sends the (possibly rewritten) body to the upstream and streams
// the response back to the client. Writes the audit entry on completion.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, body []byte, entry *AuditEntry, started time.Time) {
	target := *p.upstream
	// Strip our own /v1 prefix duplication if both upstream and incoming have it.
	target.Path = singleJoin(target.Path, r.URL.Path)
	if r.URL.RawQuery != "" {
		target.RawQuery = r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, fmt.Sprintf("dlp proxy: build upstream request: %v", err), http.StatusBadGateway)
		entry.LatencyMS = time.Since(started).Milliseconds()
		p.writeAudit(*entry)
		return
	}
	// Copy headers except hop-by-hop ones. Length is recomputed because we
	// may have changed the body during redaction.
	copyHeaders(req.Header, r.Header)
	// Let Go's transport negotiate compression and decompress for us: a
	// caller's "Accept-Encoding: gzip" would otherwise hand us a gzipped
	// body that response-side inspection can't read (silent DLP bypass).
	req.Header.Del("Accept-Encoding")
	p.applyProviderAuth(req.Header, r.URL.Path)
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

	resp, err := p.client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("dlp proxy: upstream: %v", err), http.StatusBadGateway)
		entry.LatencyMS = time.Since(started).Milliseconds()
		p.writeAudit(*entry)
		return
	}
	defer resp.Body.Close()
	if p.gatewayMode() {
		stripProviderHeaders(resp.Header)
	}

	// Decide: buffer-and-scan (response-side inspection eligible), or
	// stream through unchanged. Streaming responses (text/event-stream)
	// always pass through — see Direction docstring.
	contentType := resp.Header.Get("Content-Type")
	isStream := strings.Contains(strings.ToLower(contentType), "text/event-stream")

	if p.direction.inspectsResponse() {
		if !isStream {
			p.forwardBuffered(w, r, resp, entry, started)
			return
		}
		// Day 19: SSE streams of known shapes are redacted incrementally.
		if kind := streamKind(r.URL.Path); kind != "" {
			p.forwardStream(w, resp, kind, entry, started)
			return
		}
	}

	// Stream headers + body back to the client (pre-Day-18 behaviour).
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Flush as we go so SSE streaming responses don't buffer.
	if f, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					break
				}
				f.Flush()
			}
			if rerr != nil {
				break
			}
		}
	} else {
		_, _ = io.Copy(w, resp.Body)
	}

	entry.UpstreamCode = resp.StatusCode
	entry.LatencyMS = time.Since(started).Milliseconds()
	p.writeAudit(*entry)
}

// forwardBuffered reads the full response body, runs response-side
// inspection over it, redacts any hits, and then writes the (possibly
// rewritten) body back to the client. Only invoked for non-streaming
// responses when direction includes "response" — streaming responses
// go through the pass-through path so SSE isn't broken.
//
// The upstream is trusted to eventually close its body; we bound the
// read implicitly via the caller's request context (see http.Client's
// Timeout=0 + context cancellation).
func (p *Proxy) forwardBuffered(w http.ResponseWriter, r *http.Request, resp *http.Response, entry *AuditEntry, started time.Time) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err == nil && len(body) > maxResponseBytes {
		err = fmt.Errorf("response over %d bytes, too large to inspect", maxResponseBytes)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("dlp proxy: read upstream response: %v", err), http.StatusBadGateway)
		entry.LatencyMS = time.Since(started).Milliseconds()
		p.writeAudit(*entry)
		return
	}

	rewritten, respHits := p.inspectResponse(r.URL.Path, body)
	entry.ResponseHits = len(respHits)
	entry.ResponseLabels = Labels(respHits)

	// Escalate the overall audited action if response-side hits push
	// the decision higher (e.g. request had none, response has block).
	// Precedence stays block > redact > audit_only.
	respAction := overallAction(respHits, p.cfg.Action)
	if actionRank(respAction) > actionRank(entry.Action) {
		entry.Action = respAction
	}

	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			// We're rewriting the body — the original Content-Length
			// no longer matches. Recompute below after WriteHeader.
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rewritten)

	entry.UpstreamCode = resp.StatusCode
	entry.LatencyMS = time.Since(started).Milliseconds()
	p.writeAudit(*entry)
}

// actionRank maps an Action onto the block-wins precedence used across
// the proxy. Higher = "more serious." Keep the numbers stable — audit
// consumers rely on Action strings, but tests can (and do) sanity-check
// the ordering by ranking.
func actionRank(a Action) int {
	switch a {
	case ActionBlock:
		return 3
	case ActionRedact:
		return 2
	case ActionAuditOnly:
		return 1
	}
	return 0
}

// inspectResponse is the response-body counterpart to inspect(). Parses
// the JSON shape based on the request path (chat/completions has
// choices[].message.content; completions has choices[].text; embeddings
// carry no user-visible text). Redact-effective hits are replaced in-
// place; audit_only-effective hits are logged only; block-effective hits
// are treated as redact on the response side (we cannot un-send a 200
// once the upstream committed, and dropping the reply mid-flight would
// be worse than redacting).
func (p *Proxy) inspectResponse(path string, body []byte) ([]byte, []Hit) {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return p.inspectResponseChat(body)
	case strings.HasSuffix(path, "/messages"):
		return p.inspectResponseMessages(body)
	case strings.HasSuffix(path, "/completions"):
		return p.inspectResponseCompletions(body)
	default:
		return body, nil
	}
}

// inspectResponseChat handles {"choices":[{"message":{"role":"...","content":"..."}}]}.
func (p *Proxy) inspectResponseChat(body []byte) ([]byte, []Hit) {
	var payload map[string]interface{}
	if err := decodeJSON(body, &payload); err != nil {
		return body, nil
	}
	rawChoices, ok := payload["choices"].([]interface{})
	if !ok {
		return body, nil
	}
	var allHits []Hit
	changed := false
	for _, c := range rawChoices {
		choice, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := msg["content"].(string)
		if !ok {
			continue
		}
		hits := Scan(content, p.detectors)
		if len(hits) == 0 {
			continue
		}
		allHits = append(allHits, hits...)
		// On the response side, ALL non-audit_only hits get redacted.
		// Block on response is a nonsense (upstream already answered);
		// treat block-effective hits as redact so the caller doesn't
		// see the raw PII.
		toRedact := make([]Hit, 0, len(hits))
		for _, h := range hits {
			if effectiveAction(h, p.cfg.Action) != ActionAuditOnly {
				toRedact = append(toRedact, h)
			}
		}
		if len(toRedact) == 0 {
			continue
		}
		msg["content"] = Redact(content, toRedact)
		changed = true
	}
	if !changed {
		return body, allHits
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body, allHits
	}
	return out, allHits
}

// inspectResponseCompletions handles {"choices":[{"text":"..."}]}.
func (p *Proxy) inspectResponseCompletions(body []byte) ([]byte, []Hit) {
	var payload map[string]interface{}
	if err := decodeJSON(body, &payload); err != nil {
		return body, nil
	}
	rawChoices, ok := payload["choices"].([]interface{})
	if !ok {
		return body, nil
	}
	var allHits []Hit
	changed := false
	for _, c := range rawChoices {
		choice, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		text, ok := choice["text"].(string)
		if !ok {
			continue
		}
		hits := Scan(text, p.detectors)
		if len(hits) == 0 {
			continue
		}
		allHits = append(allHits, hits...)
		toRedact := make([]Hit, 0, len(hits))
		for _, h := range hits {
			if effectiveAction(h, p.cfg.Action) != ActionAuditOnly {
				toRedact = append(toRedact, h)
			}
		}
		if len(toRedact) == 0 {
			continue
		}
		choice["text"] = Redact(text, toRedact)
		changed = true
	}
	if !changed {
		return body, allHits
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body, allHits
	}
	return out, allHits
}

// writeAudit best-effort-records the audit entry. We never fail a request
// because the audit writer errored.
//
// A failed write is counted, logged to stderr (first failure and then every
// 100th, so a dead disk can't flood the log), reported on /healthz, and,
// with AuditRequired, turns the proxy unhealthy.
func (p *Proxy) writeAudit(e AuditEntry) {
	if p.cfg.Audit == nil {
		return
	}
	if err := p.cfg.Audit.Write(e); err != nil {
		if n := p.auditFailures.Add(1); n == 1 || n%100 == 0 {
			fmt.Fprintf(os.Stderr, "bairav dlp: AUDIT WRITE FAILED (%d so far): %v\n", n, err)
		}
	}
}

// AuditFailures is the number of audit records that could not be written.
func (p *Proxy) AuditFailures() int64 { return p.auditFailures.Load() }

// serveHealth answers /healthz locally (never forwarded, never costs a
// provider call): 200 normally, 503 when audit is required and failing.
func (p *Proxy) serveHealth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	n := p.auditFailures.Load()
	if p.cfg.AuditRequired && n > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"status":"audit_failing","audit_failures":%d}`, n)
		return
	}
	fmt.Fprintf(w, `{"status":"ok","audit_failures":%d}`, n)
}

// authorized checks the caller's token when ClientTokens is configured.
func (p *Proxy) authorized(r *http.Request) bool {
	if len(p.cfg.ClientTokens) == 0 {
		return true
	}
	got := r.Header.Get("X-Api-Key")
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		got = strings.TrimPrefix(auth, "Bearer ")
	}
	if got == "" {
		return false
	}
	ok := false
	for _, t := range p.cfg.ClientTokens {
		if subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1 {
			ok = true // no early exit: same work whichever token matches
		}
	}
	return ok
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// hopByHopHeaders are stripped between client and upstream per RFC 7230 §6.1.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHopHeaders[k] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// singleJoin joins prefix and suffix with exactly one "/" between them.
// If suffix already starts with prefix (with a trailing slash, e.g. prefix
// "/v1" + suffix "/v1/models"), it deduplicates so we don't end up hitting
// /v1/v1/models upstream.
func singleJoin(prefix, suffix string) string {
	if prefix == "" {
		return suffix
	}
	if suffix == "" {
		return prefix
	}
	// Dedupe path prefix. Compare against pTrim (no trailing /) so we treat
	// prefix "/v1" and "/v1/" the same; the return uses pTrim+remainder so
	// the "/" from the suffix is preserved (the earlier version stripped the
	// pTrim+"/" and re-concatenated prefix, which dropped the separator and
	// produced "/v1models" instead of "/v1/models").
	pTrim := strings.TrimRight(prefix, "/")
	if strings.HasPrefix(suffix, pTrim+"/") {
		return pTrim + strings.TrimPrefix(suffix, pTrim)
	}
	if !strings.HasSuffix(prefix, "/") && !strings.HasPrefix(suffix, "/") {
		return prefix + "/" + suffix
	}
	if strings.HasSuffix(prefix, "/") && strings.HasPrefix(suffix, "/") {
		return prefix + suffix[1:]
	}
	return prefix + suffix
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	body := map[string]interface{}{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Audit match modes. See Config.AuditMatches.
const (
	AuditMatchesMasked = "masked"
	AuditMatchesRaw    = "raw"
	AuditMatchesNone   = "none"
)

// auditMatches renders hits for the audit log per Config.AuditMatches.
func (p *Proxy) auditMatches(hits []Hit) []string {
	switch p.cfg.AuditMatches {
	case AuditMatchesRaw:
		return matchStrings(hits)
	case AuditMatchesNone:
		return nil
	}
	raw := matchStrings(hits)
	for i, m := range raw {
		raw[i] = MaskMatch(m)
	}
	return raw
}

// MaskMatch keeps just enough of a match for a reviewer to tell a false
// positive from a real hit without storing the value: "al…om (17)".
// Matches of 6 runes or fewer show only their length.
func MaskMatch(m string) string {
	r := []rune(m)
	if len(r) <= 6 {
		return fmt.Sprintf("… (%d)", len(r))
	}
	return fmt.Sprintf("%s…%s (%d)", string(r[:2]), string(r[len(r)-2:]), len(r))
}

func matchStrings(hits []Hit) []string {
	if len(hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Match)
	}
	return out
}
