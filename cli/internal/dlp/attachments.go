// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

import (
	"encoding/base64"
	"sort"
	"strings"
)

// Attachments: images, PDFs, audio and files inside a request.
//
// The detectors read text. A screenshot of a customer record, a scanned ID
// card or a PDF statement carries the same data as pixels or binary. The
// proxy never lets one through silently: every request that carries one is
// recorded, and the policy decides what happens. With attachments: scan,
// images are read with OCR (see ocr.go) and checked like text.

// AttachmentMode is what the proxy does with a request that carries an
// attachment.
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

	// AttachmentsScan reads inline images with OCR and runs the detectors
	// on the text: a hit blocks the request, a clean image passes.
	// Anything it can't read (PDFs, audio, files, image links, an image
	// OCR fails on) is blocked: scan never lets an unread attachment pass.
	// Experimental.
	AttachmentsScan AttachmentMode = "scan"
)

// ParseAttachmentMode validates a mode string; empty means audit.
func ParseAttachmentMode(s string) (AttachmentMode, bool) {
	switch AttachmentMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", AttachmentsAudit:
		return AttachmentsAudit, true
	case AttachmentsBlock:
		return AttachmentsBlock, true
	case AttachmentsScan:
		return AttachmentsScan, true
	}
	return "", false
}

// UninspectedHeader names the attachment kinds the DLP passed on unread.
const UninspectedHeader = "X-Bairav-DLP-Uninspected"

// ScannedHeader names the attachment kinds the DLP read with OCR and found
// nothing in.
const ScannedHeader = "X-Bairav-DLP-Scanned"

// Attachment kinds, as they appear in the audit log and the header.
const (
	KindImage = "image"
	KindPDF   = "pdf"
	KindAudio = "audio"
	KindFile  = "file"
)

// attachment is one attachment found in a request body.
type attachment struct {
	Kind string
	MIME string // "image/png", ... ("" if the request doesn't say)
	Data []byte // decoded inline data; nil for links, file ids or undecodable data
	URL  string // an http(s) link instead of inline data
}

// attachmentKinds returns the sorted, unique kinds of attachment found
// anywhere in a decoded request body.
func attachmentKinds(v interface{}) []string {
	return kindsOf(collectAttachments(v))
}

func kindsOf(as []attachment) []string {
	found := map[string]bool{}
	for _, a := range as {
		found[a.Kind] = true
	}
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// collectAttachments finds every attachment in a decoded request body. It
// knows the shapes the OpenAI Chat Completions and Responses APIs, the
// Anthropic Messages API and MCP use:
//
//	{"type":"image_url","image_url":{"url":"https://... or data:..."}}  OpenAI chat
//	{"type":"input_image","image_url":"data:..."}                     OpenAI Responses
//	{"type":"input_file" | "input_audio" | "file", ...}               OpenAI files / audio
//	{"type":"image" | "document","source":{"type":"base64" | "url"}}  Anthropic
//	{"type":"image" | "audio","data":"...","mimeType":"..."}          MCP content
//
// and, as a catch-all, any string that is a base64 data: URI.
func collectAttachments(v interface{}) []attachment {
	var out []attachment
	var walk func(interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case string:
			if isBinaryBlob(t) {
				out = append(out, fromDataURI(t, ""))
			}
		case []interface{}:
			for _, x := range t {
				walk(x)
			}
		case map[string]interface{}:
			if a, ok := blockAttachment(t); ok {
				out = append(out, a)
				return // the whole block is the attachment
			}
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(v)
	return out
}

// blockAttachment recognises a content block that carries an attachment.
func blockAttachment(m map[string]interface{}) (attachment, bool) {
	typ, _ := m["type"].(string)
	switch typ {
	case "image_url":
		// {"image_url": {"url": "..."}} or, in some clients, a plain string.
		switch u := m["image_url"].(type) {
		case map[string]interface{}:
			s, _ := u["url"].(string)
			return fromRef(s, KindImage), true
		case string:
			return fromRef(u, KindImage), true
		}
		return attachment{Kind: KindImage}, true
	case "input_image":
		s, _ := m["image_url"].(string)
		return fromRef(s, KindImage), true
	case "image":
		if src, ok := m["source"].(map[string]interface{}); ok { // Anthropic
			return fromSource(src, KindImage), true
		}
		data, _ := m["data"].(string) // MCP
		mt, _ := m["mimeType"].(string)
		return fromBase64(data, mt, KindImage), true
	case "input_audio", "audio":
		return attachment{Kind: KindAudio}, true
	case "file", "input_file":
		a := attachment{Kind: KindFile}
		if isPDFName(m["filename"]) {
			a.Kind = KindPDF
		}
		if f, ok := m["file"].(map[string]interface{}); ok && isPDFName(f["filename"]) {
			a.Kind = KindPDF
		}
		return a, true
	case "document":
		// Anthropic: a plain-text document is text and gets scanned like
		// any other string; base64 / URL / file sources are binary.
		if src, ok := m["source"].(map[string]interface{}); ok {
			if st, _ := src["type"].(string); st == "text" || st == "content" {
				return attachment{}, false
			}
			a := fromSource(src, KindPDF)
			if a.MIME != "" {
				a.Kind = kindFromMIME(a.MIME)
			}
			return a, true
		}
		return attachment{Kind: KindPDF}, true
	case "resource":
		// MCP embedded resource: text is scanned, a binary blob is not.
		if r, ok := m["resource"].(map[string]interface{}); ok {
			if blob, ok := r["blob"].(string); ok {
				mt, _ := r["mimeType"].(string)
				return fromBase64(blob, mt, kindFromMIME(mt)), true
			}
		}
	}
	return attachment{}, false
}

func isPDFName(v interface{}) bool {
	s, _ := v.(string)
	return strings.HasSuffix(strings.ToLower(s), ".pdf")
}

// fromRef reads an image reference: a data: URI or a link.
func fromRef(s, kind string) attachment {
	switch {
	case isBinaryBlob(s):
		return fromDataURI(s, kind)
	case strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://"):
		return attachment{Kind: kind, URL: s}
	}
	return attachment{Kind: kind}
}

// fromSource reads an Anthropic source block.
func fromSource(src map[string]interface{}, kind string) attachment {
	st, _ := src["type"].(string)
	mt, _ := src["media_type"].(string)
	switch st {
	case "base64":
		data, _ := src["data"].(string)
		return fromBase64(data, mt, kind)
	case "url":
		u, _ := src["url"].(string)
		return attachment{Kind: kind, MIME: mt, URL: u}
	}
	return attachment{Kind: kind, MIME: mt}
}

// fromDataURI decodes "data:image/png;base64,....". kind "" = from the MIME type.
func fromDataURI(s, kind string) attachment {
	meta, payload, _ := strings.Cut(strings.TrimPrefix(s, "data:"), ",")
	mt, _, _ := strings.Cut(meta, ";")
	if kind == "" {
		kind = kindFromMIME(mt)
	}
	return fromBase64(payload, mt, kind)
}

func fromBase64(data, mt, kind string) attachment {
	a := attachment{Kind: kind, MIME: strings.ToLower(mt)}
	if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data)); err == nil && len(b) > 0 {
		a.Data = b
	} else if b, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(data)); err == nil && len(b) > 0 {
		a.Data = b
	}
	return a
}

// kindFromMIME maps "image/png", "application/pdf" etc. to a kind.
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
