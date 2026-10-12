// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChat_NonStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var got ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Stream {
			t.Error("Chat() should set Stream=false")
		}
		if len(got.Messages) != 1 || got.Messages[0].Content != "hi" {
			t.Errorf("unexpected messages: %+v", got.Messages)
		}
		_, _ = fmt.Fprintln(w, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello!"}}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL + "/v1")
	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "hello!" {
		t.Errorf("unexpected reply: %q", resp.Choices[0].Message.Content)
	}
}

func TestChatStream_DeltaCollection(t *testing.T) {
	// Simulate a 3-chunk SSE stream.
	chunks := []string{
		`{"choices":[{"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"delta":{"content":", "}}]}`,
		`{"choices":[{"delta":{"content":"world!"}}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, c := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	defer srv.Close()

	c := New(srv.URL + "/v1")
	var collected strings.Builder
	err := c.ChatStream(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "say hi"}},
	}, func(delta string) error {
		collected.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := collected.String(); got != "Hello, world!" {
		t.Errorf("collected delta = %q, want %q", got, "Hello, world!")
	}
}

func TestChatStream_HandlesBlankAndMalformedLines(t *testing.T) {
	// Real backends send keepalives, blank lines, and occasionally bad chunks.
	// The client must skip them without aborting the stream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, "")
		_, _ = fmt.Fprintln(w, ": keepalive")
		_, _ = fmt.Fprintln(w, "data: not-valid-json")
		_, _ = fmt.Fprintln(w, "")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	defer srv.Close()

	c := New(srv.URL + "/v1")
	var got string
	err := c.ChatStream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "x"}},
	}, func(delta string) error {
		got += delta
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got != "ok" {
		t.Errorf("got %q, want %q", got, "ok")
	}
}

func TestChat_HTTPErrorSurfacesMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL + "/v1")
	_, err := c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("expected error on 503")
	}
	if !strings.Contains(err.Error(), "model not loaded") {
		t.Errorf("expected upstream message in error, got %v", err)
	}
}
