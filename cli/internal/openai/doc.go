// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package openai is a tiny OpenAI Chat Completions client — just enough to
// drive `bairav chat` against any backend that exposes /v1/chat/completions.
//
// We don't depend on the official openai-go SDK because:
//  1. We only need one method (chat with streaming) — pulling the SDK in
//     adds ~10 transitive deps for ~200 lines of value.
//  2. The SDK's auth model assumes OpenAI-cloud + key. Self-hosted backends
//     need either no auth or a custom header — easier to just write it.
//  3. Smaller dep surface = smaller `bairav` binary, faster cold-start.
//
// If we ever need embeddings, fine-tuning, or assistants, switch to the SDK.
package openai
