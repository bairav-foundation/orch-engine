// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to an OpenAI-compatible Chat Completions endpoint.
//
// BaseURL is the "/v1" root, e.g. "http://localhost:8080/v1". APIKey is
// optional — most self-hosted backends accept any value (including empty).
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// New returns a Client with sensible defaults.
func New(baseURL string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// Chat sends a non-streaming completion request and returns the assistant's
// full reply.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.applyAuth(httpReq)

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("chat completions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("chat completions: %s — %s",
			resp.Status, strings.TrimSpace(string(errBody)))
	}
	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

// ChatStream sends a streaming completion request and invokes onDelta for
// every incremental token chunk. Returns when the upstream sends [DONE], the
// context is cancelled, or an error occurs.
//
// The OpenAI streaming protocol is Server-Sent Events: the response body is
// a series of "data: {json}\n\n" lines, terminated by "data: [DONE]".
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, onDelta func(content string) error) error {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.applyAuth(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

	// No timeout on the HTTP client for streaming — the caller's ctx controls.
	streamingClient := &http.Client{Timeout: 0}
	resp, err := streamingClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("chat completions stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("chat completions stream: %s — %s",
			resp.Status, strings.TrimSpace(string(errBody)))
	}

	scanner := bufio.NewScanner(resp.Body)
	// Default scanner buffer is 64KB; long completions can exceed it on a
	// single SSE line. Bump to 1MB.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Text()
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			return nil
		}
		var chunk StreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// Some backends emit keepalive lines or malformed chunks; skip.
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		if err := onDelta(delta); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	return nil
}

// applyAuth sets the Authorization header if APIKey is non-empty.
func (c *Client) applyAuth(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bairav/0.1 (+https://bairavs.com)")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
}
