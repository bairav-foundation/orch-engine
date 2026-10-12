// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package hf is a minimal Hugging Face Hub client for downloading model files.
//
// Scope is intentionally small: list the files in a public or private repo,
// resolve their blob URLs, and stream them to a content-addressed local cache
// with Range-based resume support. We do not depend on huggingface_hub or any
// third-party HF library — the API surface we need is three endpoints, and
// stdlib net/http handles the rest.
//
// The cache layout is:
//
//	$BAIRAV_CACHE_DIR (or ~/.bairav/models) /
//	    <org>/
//	        <repo>/
//	            <file-path-from-hf>
//
// Files are written atomically: download to <name>.partial, fsync, rename.
// Re-running pull on an existing complete file is a no-op.
package hf
