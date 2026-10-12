// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package serve provides a backend-agnostic interface for running an LLM
// inference engine locally and exposing an OpenAI-compatible HTTP endpoint.
//
// The Backend interface is the abstraction that lets us swap inference engines
// (llama.cpp's llama-server today; vLLM, Ollama, MLX, sglang in future) without
// touching the CLI commands or downstream packages. As long as a Backend speaks
// the OpenAI shape on its bound port, BAIRAVS doesn't care what runs behind it.
//
// See docs/ARCHITECTURE.md "Backend strategy" for the long-term vision.
package serve
