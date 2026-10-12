// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package llamaserver implements a serve.Backend that shells out to the
// llama-server binary from llama.cpp.
//
// llama-server speaks the OpenAI Chat Completions API natively on its bound
// port, so BAIRAVS doesn't need to wrap the response shape. We supervise the
// process: spawn, wait-ready by polling /health, and SIGTERM (then SIGKILL)
// on Stop.
//
// Install on macOS:    brew install llama.cpp
// Install on Linux:    https://github.com/ggml-org/llama.cpp/releases
package llamaserver
