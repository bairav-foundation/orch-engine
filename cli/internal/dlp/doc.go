// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package dlp implements the prompt-side Data Loss Prevention layer that
// sits in front of any OpenAI-compatible model endpoint.
//
// The shape is intentionally small for v1:
//
//   - A Detector is a named function that finds spans of PII/secrets in
//     plain text. Built-ins cover email, US SSN, credit card with Luhn,
//     US phone, and AWS access keys.
//   - Scan runs every detector and returns deduplicated, non-overlapping
//     Hits sorted left-to-right.
//   - Redact rewrites text by replacing each Hit's span with
//     [REDACTED:LABEL]. Operates back-to-front so byte offsets stay valid.
//   - The proxy package (proxy.go) wires the detectors into an HTTP
//     reverse-proxy that intercepts chat/completion requests, rewrites
//     or blocks per policy, and writes one JSONL audit record per call.
//
// Out of scope for v1: response-side redaction (model regurgitating PII
// it saw in training), cross-message correlation, encrypted-payload
// inspection, OCR on image inputs. These belong in v1.1+.
//
// Why not a third-party library: we want zero external runtime deps and
// a vendored detector set we can tune for our customers. Most off-the-
// shelf PII libraries are LGPL/AGPL or have large transitive deps.
package dlp
