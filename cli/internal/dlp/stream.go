// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// streamHoldback is how many bytes of generated text the streaming
// redactor keeps unemitted at the tail of each choice's buffer. A PII
// span that straddles two SSE chunks ("jane@co" | "rp.com") is only
// detectable once both halves are in the buffer, so we never emit the
// last streamHoldback bytes until more text arrives or the choice ends.
//
// 128 bytes covers every built-in and pack detector with room to spare
// (the longest realistic match — a long email address — is well under
// that). Cost: the caller sees text ~128 bytes "late"; total latency
// to the final token is unchanged because the tail is flushed on
// finish_reason / [DONE].
const streamHoldback = 128

// streamKind reports which OpenAI streaming shape a path produces, or ""
// when the proxy doesn't know how to redact that stream (pass-through).
func streamKind(path string) string {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return "chat"
	case strings.HasSuffix(path, "/completions"):
		return "completion"
	case strings.HasSuffix(path, "/messages"):
		return "messages" // Anthropic Messages SSE (Day 28)
	}
	return ""
}

// streamRedactor redacts OpenAI SSE streams incrementally. It keeps one
// text buffer per choice index, scans the buffer on every chunk, emits
// only the prefix that can no longer be part of an undetected span, and
// flushes the remainder when the choice finishes.
type streamRedactor struct {
	p    *Proxy
	kind string // "chat" | "completion"
	bufs map[int]string
	// hits are counted once, at the moment the span is emitted, so
	// re-scanning the same buffer across chunks never double counts.
	hits []Hit
	// pendingEvent holds an Anthropic "event:" line until its data line
	// arrives, so held-back text can be flushed as a complete event
	// *before* content_block_stop (messages kind only).
	pendingEvent string
}

func newStreamRedactor(p *Proxy, kind string) *streamRedactor {
	return &streamRedactor{p: p, kind: kind, bufs: map[int]string{}}
}

// drain returns the redacted text that is safe to emit for choice idx.
// When final is true the whole buffer is emitted.
func (s *streamRedactor) drain(idx int, final bool) string {
	buf := s.bufs[idx]
	if buf == "" {
		return ""
	}
	hits := Scan(buf, s.p.detectors)

	cut := len(buf)
	if !final {
		cut = len(buf) - streamHoldback
		if cut < 0 {
			cut = 0
		}
		// Never cut through a detected span — pull the cut back to the
		// span's start and repeat until stable (overlapping candidates).
		for moved := true; moved; {
			moved = false
			for _, h := range hits {
				if h.Start < cut && h.End > cut {
					cut = h.Start
					moved = true
				}
			}
		}
		// Keep the cut on a UTF-8 boundary so we never split a rune.
		for cut > 0 && cut < len(buf) && !utf8.RuneStart(buf[cut]) {
			cut--
		}
	}

	var emitted, redactable []Hit
	for _, h := range hits {
		if h.End <= cut {
			emitted = append(emitted, h)
			// Response side: anything but audit_only gets redacted — see
			// inspectResponse for why block degrades to redact here.
			if effectiveAction(h, s.p.cfg.Action) != ActionAuditOnly {
				redactable = append(redactable, h)
			}
		}
	}
	s.hits = append(s.hits, emitted...)
	out := Redact(buf[:cut], redactable)
	s.bufs[idx] = buf[cut:]
	return out
}

// processPayload rewrites one parsed SSE JSON chunk in place. Returns
// true when the payload was modified and must be re-serialised.
func (s *streamRedactor) processPayload(payload map[string]interface{}) bool {
	choices, ok := payload["choices"].([]interface{})
	if !ok {
		return false
	}
	changed := false
	for pos, c := range choices {
		choice, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		idx := pos
		if f, ok := choice["index"].(float64); ok {
			idx = int(f)
		}

		// Locate the text field for this stream shape.
		var container map[string]interface{}
		var field string
		switch s.kind {
		case "chat":
			delta, ok := choice["delta"].(map[string]interface{})
			if !ok {
				delta = map[string]interface{}{}
			}
			container, field = delta, "content"
			choice["delta"] = delta
		case "completion":
			container, field = choice, "text"
		}

		text, hasText := container[field].(string)
		finished := choice["finish_reason"] != nil

		if !hasText && !finished {
			continue
		}
		if hasText {
			s.bufs[idx] += text
		}
		out := s.drain(idx, finished)
		if hasText || out != "" {
			container[field] = out
			changed = true
		}
	}
	return changed
}

// flushAll emits any still-buffered text as synthetic SSE frames. Called
// on [DONE] and on upstream EOF so a stream that never sends a
// finish_reason still delivers every (redacted) byte.
func (s *streamRedactor) flushAll() string {
	idxs := make([]int, 0, len(s.bufs))
	for i, b := range s.bufs {
		if b != "" {
			idxs = append(idxs, i)
		}
	}
	sort.Ints(idxs)
	var sb strings.Builder
	for _, i := range idxs {
		if s.kind == "messages" {
			sb.WriteString(s.messagesFlushFrame(i))
			continue
		}
		text := s.drain(i, true)
		if text == "" {
			continue
		}
		var frame map[string]interface{}
		if s.kind == "chat" {
			frame = map[string]interface{}{
				"object": "chat.completion.chunk",
				"choices": []interface{}{map[string]interface{}{
					"index": i, "delta": map[string]interface{}{"content": text}, "finish_reason": nil,
				}},
			}
		} else {
			frame = map[string]interface{}{
				"choices": []interface{}{map[string]interface{}{
					"index": i, "text": text, "finish_reason": nil,
				}},
			}
		}
		b, err := json.Marshal(frame)
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "data: %s\n\n", b)
	}
	if s.pendingEvent != "" { // stream ended between an event: and its data:
		sb.WriteString(s.pendingEvent)
		s.pendingEvent = ""
	}
	return sb.String()
}

// processLine transforms one raw SSE line. Non-data lines (comments,
// "event:", blank separators) pass through untouched.
func (s *streamRedactor) processLine(line string) string {
	if s.kind == "messages" {
		return s.processMessagesLine(line)
	}
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		return line
	}
	data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if data == "[DONE]" {
		return s.flushAll() + line
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return line
	}
	if !s.processPayload(payload) {
		return line
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return line
	}
	return "data: " + string(b) + "\n"
}

// forwardStream relays an SSE response line by line through a
// streamRedactor, flushing after every line so the caller still sees
// tokens incrementally.
func (p *Proxy) forwardStream(w http.ResponseWriter, resp *http.Response, kind string, entry *AuditEntry, started time.Time) {
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue // body is rewritten; length unknown up front
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	sr := newStreamRedactor(p, kind)
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if _, werr := io.WriteString(w, sr.processLine(line)); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if tail := sr.flushAll(); tail != "" {
		_, _ = io.WriteString(w, tail)
		if flusher != nil {
			flusher.Flush()
		}
	}

	entry.ResponseHits = len(sr.hits)
	entry.ResponseLabels = Labels(sr.hits)
	if respAction := overallAction(sr.hits, p.cfg.Action); len(sr.hits) > 0 && actionRank(respAction) > actionRank(entry.Action) {
		entry.Action = respAction
	}
	entry.UpstreamCode = resp.StatusCode
	entry.LatencyMS = time.Since(started).Milliseconds()
	p.writeAudit(*entry)
}

// -----------------------------------------------------------------------------
// Anthropic Messages streams (Day 28)
//
//	event: content_block_delta
//	data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}
//
//	event: content_block_stop
//	data: {"type":"content_block_stop","index":0}
//
// text_delta text is held back and redacted exactly like OpenAI deltas. The
// tail of a block is emitted as an extra content_block_delta event just
// before that block's content_block_stop. input_json_delta (streamed tool
// arguments) is partial JSON and passes through; tool inputs are scanned
// on the request side when they come back as tool_result / tool_use.
// -----------------------------------------------------------------------------

func (s *streamRedactor) processMessagesLine(line string) string {
	trimmed := strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(trimmed, "event:"):
		s.pendingEvent = line
		return ""
	case !strings.HasPrefix(trimmed, "data:"):
		ev := s.pendingEvent
		s.pendingEvent = ""
		return ev + line
	}
	ev := s.pendingEvent
	s.pendingEvent = ""
	data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return ev + line
	}
	idx := 0
	if f, ok := payload["index"].(float64); ok {
		idx = int(f)
	}
	switch payload["type"] {
	case "content_block_delta":
		delta, _ := payload["delta"].(map[string]interface{})
		if delta == nil || delta["type"] != "text_delta" {
			return ev + line
		}
		text, _ := delta["text"].(string)
		s.bufs[idx] += text
		delta["text"] = s.drain(idx, false)
		b, err := json.Marshal(payload)
		if err != nil {
			return ev + line
		}
		return ev + "data: " + string(b) + "\n"
	case "content_block_stop":
		return s.messagesFlushFrame(idx) + ev + line
	}
	return ev + line
}

// messagesFlushFrame drains block idx completely and wraps the text in a
// standalone content_block_delta event ("" when nothing is buffered).
func (s *streamRedactor) messagesFlushFrame(idx int) string {
	text := s.drain(idx, true)
	if text == "" {
		return ""
	}
	b, err := json.Marshal(map[string]interface{}{
		"type": "content_block_delta", "index": idx,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	})
	if err != nil {
		return ""
	}
	return "event: content_block_delta\ndata: " + string(b) + "\n\n"
}
