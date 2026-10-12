// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"bytes"
	"context"
	_ "embed" // for the go:embed directives below
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed assets/index.html
var indexHTML []byte

//go:embed assets/demo.json
var demoJSON []byte

// framesPlaceholder is the token in index.html replaced by inlined demo
// frames. Left as `null` in live mode, the page polls /api/snapshot.
const framesPlaceholder = "/*BAIRAV_FRAMES*/null"

// Options configures Handler.
type Options struct {
	// Demo serves the scripted demo frames instead of live data.
	Demo bool
	// Snapshot builds a live snapshot. Required unless Demo is set.
	Snapshot func(ctx context.Context) Snapshot
}

// DemoFrames parses and returns the embedded demo script. Exported so the
// CLI and tests can validate it; a malformed demo.json is a build bug.
func DemoFrames() ([]DemoFrame, error) {
	var frames []DemoFrame
	if err := json.Unmarshal(demoJSON, &frames); err != nil {
		return nil, fmt.Errorf("dashboard: embedded demo.json: %w", err)
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("dashboard: embedded demo.json has no frames")
	}
	return frames, nil
}

// renderIndex returns index.html with demo frames inlined (frames != nil)
// or with a bare `null` in live mode. The placeholder itself must never
// reach the browser: "/*…*/null" is not valid JSON, and the page's
// JSON.parse of it threw before anything rendered (a real bug: the live
// dashboard came up empty). json.Marshal escapes <, > and &, so inlined
// frames can't close the surrounding <script> tag.
func renderIndex(frames []DemoFrame) ([]byte, error) {
	if !bytes.Contains(indexHTML, []byte(framesPlaceholder)) {
		return nil, fmt.Errorf("dashboard: index.html is missing the frames placeholder")
	}
	b := []byte("null")
	if frames != nil {
		var err error
		if b, err = json.Marshal(frames); err != nil {
			return nil, err
		}
	}
	return bytes.Replace(indexHTML, []byte(framesPlaceholder), b, 1), nil
}

// LoopbackOnly wraps h so it only answers requests whose Host header is a
// loopback name. A dashboard bound to 127.0.0.1 is otherwise reachable from
// any web page via DNS rebinding (evil.example resolving to 127.0.0.1),
// which could read /api/snapshot: everything your kubeconfig can see.
func LoopbackOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if hp, _, err := net.SplitHostPort(host); err == nil {
			host = hp
		}
		host = strings.Trim(host, "[]")
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "dashboard: unexpected Host header", http.StatusMisdirectedRequest)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Handler returns the dashboard's HTTP handler: GET / (the UI) and, in
// live mode, GET /api/snapshot. Everything is read-only.
func Handler(opts Options) (http.Handler, error) {
	var frames []DemoFrame
	if opts.Demo {
		f, err := DemoFrames()
		if err != nil {
			return nil, err
		}
		frames = f
	} else if opts.Snapshot == nil {
		return nil, fmt.Errorf("dashboard: Snapshot func is required in live mode")
	}
	page, err := renderIndex(frames)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		secureHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	if !opts.Demo {
		mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "read-only", http.StatusMethodNotAllowed)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			secureHeaders(w)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(opts.Snapshot(ctx))
		})
	}
	return mux, nil
}

// secureHeaders: the page is fully self-contained, so it may load nothing
// from anywhere else. Inline script/style are needed (single-file UI).
func secureHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

// Export writes the demo as one self-contained index.html into dir — no
// server, no network, no external assets. Host it anywhere.
func Export(dir string) (string, error) {
	frames, err := DemoFrames()
	if err != nil {
		return "", err
	}
	page, err := renderIndex(frames)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "index.html")
	if err := os.WriteFile(path, page, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
