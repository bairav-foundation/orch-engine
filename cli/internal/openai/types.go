// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package openai

// Message is one chat turn. Role is "system", "user", or "assistant".
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the body of POST /v1/chat/completions.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

// ChatResponse is the non-streaming response shape.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage,omitempty"`
}

// Choice is one completion variant in the response.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message,omitempty"`
	Delta        Delta   `json:"delta,omitempty"` // streaming only
	FinishReason string  `json:"finish_reason,omitempty"`
}

// Delta carries one streaming chunk: the incremental token slice for this turn.
type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// Usage reports token accounting. Most self-hosted backends fill this in.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk is one Server-Sent Event line decoded from /v1/chat/completions
// when stream:true. The OpenAI shape reuses ChatResponse with Choices[].Delta
// populated instead of Choices[].Message.
type StreamChunk = ChatResponse
