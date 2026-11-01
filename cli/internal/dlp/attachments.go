// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"sort"
	"strings"
)

// Attachments: images, PDFs, audio and files inside a request.
//
// The detectors read text. A screenshot of a customer record, a scanned ID
// card or a PDF statement carries the same data as pixels or binary, which
// the detectors can't see. Until the proxy can read them (OCR is planned),
// it must never let them through silently: every request that carries one
// is recorded as "uninspected", and the policy decides whether it may pass.

// AttachmentMode is what the proxy does with a request that carries an
// attachment it can't inspect.
type AttachmentMode string

const (
	// AttachmentsAudit forwards the request, records the attachment kinds
	// in the audit log ("uninspected") and tells the caller in the
	// X-Bairav-DLP-Uninspected response header. The default, so existing
	// vision use keeps working.
	AttachmentsAudit AttachmentMode = "audit"

	// AttachmentsBlock refuses the request with HTTP 403. For teams where
	// nothing may reach a model unless the DLP has read it.
	AttachmentsBlock AttachmentMode = "block"
)

// ParseAttachmentMode validates a mode string; empty means audit.
func ParseAttachmentMode(s string) (AttachmentMode, bool) {
	switch AttachmentMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", AttachmentsAudit:
		return AttachmentsAudit, true
	case AttachmentsBlock:
		return AttachmentsBlock, true
	}
	return "", false
}

// UninspectedHeader names the attachment kinds the DLP passed on unread.
const UninspectedHeader = "X-Bairav-DLP-Uninspected"

// Attachment kinds, as they appear in the audit log and the header.
const (
	KindImage = "image"
	KindPDF   = "pdf"
	KindAudio = "audio"
	KindFile  = "file"
)

// attachmentKinds returns the sorted, unique kinds of attachment found
// anywhere in a decoded request body. It knows the shapes the OpenAI Chat
// Completions and Responses APIs, the Anthropic Messages API and MCP use:
//
//	{"type":"image_url","image_url":{"url":"https://... or data:..."}}  OpenAI chat
//	{"type":"input_image" | "input_file" | "input_audio", ...}       OpenAI Responses / audio
//	{"type":"file","file":{"file_data" | "file_id": ...}}             OpenAI chat files
//	{"type":"image" | "document","source":{"type":"base64" | "url"}}  Anthropic
//	{"type":"image" | "audio","data":"...","mimeType":"..."}          MCP content
//
// and, as a catch-all, any string that is a base64 data: URI.
func attachmentKinds(v interface{}) []string {
	found := map[string]bool{}
	var walk func(interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case string:
			if isBinaryBlob(t) {
				found[kindFromMIME(t[len("data:"):])] = true
			}
		case []interface{}:
			for _, x := range t {
				walk(x)
			}
		case map[string]interface{}:
			if k := blockKind(t); k != "" {
				found[k] = true
				return // the whole block is the attachment
			}
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(v)
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// blockKind recognises a content block that carries an attachment, or "".
func blockKind(m map[string]interface{}) string {
	typ, _ := m["type"].(string)
	switch typ {
	case "image_url", "input_image", "image":
		return KindImage
	case "input_audio", "audio":
		return KindAudio
	case "file", "input_file":
		if s, ok := m["filename"].(string); ok && strings.HasSuffix(strings.ToLower(s), ".pdf") {
			return KindPDF
		}
		if f, ok := m["file"].(map[string]interface{}); ok {
			if s, ok := f["filename"].(string); ok && strings.HasSuffix(strings.ToLower(s), ".pdf") {
				return KindPDF
			}
		}
		return KindFile
	case "document":
		// Anthropic: a plain-text document is text and gets scanned like
		// any other string; base64 / URL / file sources are binary.
		if src, ok := m["source"].(map[string]interface{}); ok {
			if st, _ := src["type"].(string); st == "text" || st == "content" {
				return ""
			}
			if mt, _ := src["media_type"].(string); mt != "" {
				return kindFromMIME(mt)
			}
		}
		return KindPDF
	case "resource":
		// MCP embedded resource: text is scanned, a binary blob is not.
		if r, ok := m["resource"].(map[string]interface{}); ok {
			if _, ok := r["blob"]; ok {
				mt, _ := r["mimeType"].(string)
				return kindFromMIME(mt)
			}
		}
	}
	return ""
}

// kindFromMIME maps "image/png", "application/pdf;base64,..." etc. to a kind.
func kindFromMIME(mt string) string {
	mt = strings.ToLower(mt)
	switch {
	case strings.HasPrefix(mt, "image/"):
		return KindImage
	case strings.HasPrefix(mt, "application/pdf"):
		return KindPDF
	case strings.HasPrefix(mt, "audio/"):
		return KindAudio
	}
	return KindFile
}
