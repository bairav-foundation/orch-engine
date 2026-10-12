// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bairav-foundation/orch-engine/cli/internal/compose"
	"github.com/bairav-foundation/orch-engine/cli/internal/openai"
)

// chatCmd is the interactive client for talking to a running BAIRAVS-served model.
//
// Real implementation (Day 3-4): connects to localhost:8080/v1/chat/completions,
// streams responses chunk-by-chunk to the terminal, maintains conversation
// history in memory for the session.
var chatCmd = &cobra.Command{
	Use:   "chat [model-name]",
	Short: "Chat interactively with a running model",
	Long: `Open an interactive chat session against a BAIRAVS-served model on
the configured endpoint (default http://localhost:8080/v1). Streams tokens
to the terminal as the model produces them.

Typical flow:
  bairav model serve Qwen/Qwen2.5-0.5B-Instruct-GGUF &
  bairav chat Qwen/Qwen2.5-0.5B-Instruct-GGUF

If model-name is omitted, the local server's default model is used (most
backends accept any model name when only one is loaded).

REPL controls:
  /exit, /quit, Ctrl-D    — leave the session
  /reset                  — clear conversation history (start fresh)
  /system <prompt>        — replace the system prompt mid-session`,
	Args: cobra.MaximumNArgs(1),
	RunE: runChat,
}

func init() {
	chatCmd.Flags().String("endpoint", "http://localhost:8080/v1",
		"OpenAI-compatible API base URL")
	chatCmd.Flags().String("system", "You are a helpful assistant.",
		"system prompt to seed the conversation")
	chatCmd.Flags().Float64("temperature", 0.7,
		"sampling temperature (0 = deterministic, 1 = creative)")
	chatCmd.Flags().Duration("wait", 0,
		"poll the endpoint until it's ready before entering the REPL (0 = no wait). "+
			"Useful right after `bairav model deploy` when the container is still pulling the GGUF.")
}

func runChat(cmd *cobra.Command, args []string) error {
	endpoint, _ := cmd.Flags().GetString("endpoint")
	systemPrompt, _ := cmd.Flags().GetString("system")
	temperature, _ := cmd.Flags().GetFloat64("temperature")
	wait, _ := cmd.Flags().GetDuration("wait")

	modelName := "default"
	if len(args) == 1 {
		modelName = args[0]
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client := openai.New(endpoint)
	out := cmd.OutOrStdout()

	// Conversation history kept in memory for the session.
	history := []openai.Message{{Role: "system", Content: systemPrompt}}

	fmt.Fprintf(out, "bairav chat — endpoint=%s model=%s\n", endpoint, modelName)

	// If the user asked us to wait, just poll until ready. We do NOT branch
	// on the initial probe state — the user opted in to waiting, so the
	// only question is whether the endpoint comes alive within the budget.
	// This also dodges the macOS Docker Desktop quirk where a not-yet-ready
	// container returns "connection refused" instead of RST.
	if wait > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"waiting up to %s for %s to become ready ...\n", wait, endpoint)
		if waitUntilReady(ctx, endpoint, wait) {
			fmt.Fprintln(cmd.ErrOrStderr(), "✓ model is ready.")
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"⚠ still not ready after %s. Continuing into the REPL anyway; check `bairav ps` or `docker logs bairav-<name>`.\n", wait)
		}
	} else {
		// No --wait: probe once, classify with help from the docker stack
		// list so we can tell "nothing here" from "stack up, model loading"
		// from "this URL only resolves inside the cluster".
		switch s := classifyEndpoint(ctx, endpoint); s {
		case endpointReady:
			// happy path, no message
		case endpointPortOpenButNotReady:
			fmt.Fprintf(cmd.ErrOrStderr(),
				"⚠ %s is mapped to a bairav stack but the model isn't ready yet (the container is still pulling/loading the GGUF).\n"+
					"   re-run with --wait 5m   to block until ready\n"+
					"   inspect logs:           docker logs -f bairav-<name>\n",
				endpoint)
		case endpointInClusterDNS:
			fmt.Fprintf(cmd.ErrOrStderr(),
				"⚠ %s is a Kubernetes in-cluster DNS name — only reachable from inside the cluster.\n"+
					"   port-forward to talk to it from your laptop, e.g.:\n"+
					"     kubectl -n <namespace> port-forward svc/<service-name> 8080:8080\n"+
					"   then chat against http://127.0.0.1:8080/v1\n"+
					"   (use `bairav ps` to see the service name + namespace)\n",
				endpoint)
		case endpointUnreachable:
			fmt.Fprintf(cmd.ErrOrStderr(),
				"⚠ no server reachable at %s yet.\n"+
					"   start one in another terminal: bairav model serve <hf-repo>\n"+
					"                                  bairav model deploy <hf-repo>\n"+
					"   see what's running:           bairav ps\n",
				endpoint)
		}
	}

	fmt.Fprintln(out, "Type a message and hit Enter. /exit to leave, /reset to clear history.")
	fmt.Fprintln(out)

	scanner := bufio.NewScanner(cmd.InOrStdin())
	// Allow long pasted prompts (default scanner cap is 64KB).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		fmt.Fprint(out, "you> ")
		if !scanner.Scan() {
			// Ctrl-D / EOF.
			fmt.Fprintln(out)
			return nil
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		// Slash-command handling.
		switch {
		case input == "/exit" || input == "/quit":
			return nil
		case input == "/reset":
			history = []openai.Message{{Role: "system", Content: systemPrompt}}
			fmt.Fprintln(out, "(history cleared)")
			continue
		case strings.HasPrefix(input, "/system "):
			systemPrompt = strings.TrimSpace(strings.TrimPrefix(input, "/system "))
			history = []openai.Message{{Role: "system", Content: systemPrompt}}
			fmt.Fprintf(out, "(system prompt updated; history reset)\n")
			continue
		case strings.HasPrefix(input, "/"):
			fmt.Fprintf(out, "(unknown command %q — try /exit, /reset, /system)\n", input)
			continue
		}

		history = append(history, openai.Message{Role: "user", Content: input})

		fmt.Fprint(out, "bot> ")
		var assistantReply strings.Builder
		err := client.ChatStream(ctx, openai.ChatRequest{
			Model:       modelName,
			Messages:    history,
			Temperature: temperature,
		}, func(delta string) error {
			assistantReply.WriteString(delta)
			fmt.Fprint(out, delta)
			return nil
		})
		fmt.Fprintln(out)

		if err != nil {
			// Show the error but stay in the REPL — transient backend issues
			// shouldn't kill the session. Drop the user's message from history
			// since there was no successful reply.
			//
			// Classify by docker-stack knowledge first: if any bairav stack
			// is mapped to this endpoint's port, treat the failure as "model
			// not ready yet" regardless of error string. That's the right
			// hint cross-platform (Linux RST, macOS conn-refused, etc.).
			switch {
			case isInClusterDNS(endpoint):
				fmt.Fprintf(cmd.ErrOrStderr(),
					"(error: %s is a Kubernetes in-cluster DNS name — only reachable from inside the cluster)\n"+
						"  port-forward to talk to it from your laptop:\n"+
						"    kubectl -n <namespace> port-forward svc/<service-name> 8080:8080\n"+
						"  then chat against http://127.0.0.1:8080/v1\n",
					endpoint)
			case endpointMatchesKnownStack(ctx, endpoint):
				fmt.Fprintf(cmd.ErrOrStderr(),
					"(error: %s is mapped to a bairav stack but the model isn't ready yet)\n"+
						"  the container is likely still pulling/loading the GGUF — give it a minute.\n"+
						"  check status:         bairav ps\n"+
						"  follow inside docker: docker logs -f bairav-<name>\n"+
						"  or re-launch chat with --wait 5m to block until ready\n",
					endpoint)
			case isConnRefused(err):
				fmt.Fprintf(cmd.ErrOrStderr(),
					"(error: no server at %s)\n"+
						"  start one with: bairav model serve <hf-repo>\n"+
						"                  bairav model deploy <hf-repo>\n"+
						"  see running:    bairav ps\n",
					endpoint)
			case isNotReady(err):
				// RST/EOF without a matching stack — local serve still loading.
				fmt.Fprintf(cmd.ErrOrStderr(),
					"(error: %s is reachable but the model isn't ready yet)\n"+
						"  give it a minute; check `bairav ps`.\n",
					endpoint)
			default:
				fmt.Fprintf(cmd.ErrOrStderr(), "(error: %v)\n", err)
			}
			history = history[:len(history)-1]
			if ctx.Err() != nil {
				return nil
			}
			continue
		}

		history = append(history, openai.Message{
			Role:    "assistant",
			Content: assistantReply.String(),
		})
	}
}

// endpointState is the user-facing classification of an endpoint.
type endpointState int

const (
	endpointUnreachable         endpointState = iota // nothing here
	endpointPortOpenButNotReady                      // bairav docker stack is mapped here, but the model isn't ready yet
	endpointReady                                    // /models returned 200
	endpointInClusterDNS                             // *.svc.cluster.local URL — only reachable from inside the cluster
)

// probeReady is the basic readiness check: 200 from /models within 2 seconds.
// Used directly by waitUntilReady; classifyEndpoint wraps it with a
// docker-stack lookup for the no-wait path's user-facing classification.
func probeReady(parent context.Context, baseURL string) bool {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// classifyEndpoint figures out what to tell the user about the endpoint.
// Only used on the no-wait path; --wait skips this and just polls.
//
// Order matters:
//  1. *.svc.cluster.local is a k8s in-cluster DNS name — only reachable
//     from inside a pod. We surface that first so the user gets the right
//     hint (kubectl port-forward) instead of generic "no server here".
//  2. Probe /models — if it answers, we're done.
//  3. Docker-stack lookup. Cross-platform note: on Linux, "container up,
//     llama-server not bound" returns connection-reset; on macOS (Docker
//     Desktop / vpnkit) the same scenario returns connection-refused. We
//     can't tell them apart from the wire, so we ask docker directly. We
//     ONLY count a match when the URL host is loopback — port matching
//     alone would false-positive against any URL on port 8080.
func classifyEndpoint(parent context.Context, baseURL string) endpointState {
	if isInClusterDNS(baseURL) {
		return endpointInClusterDNS
	}
	if probeReady(parent, baseURL) {
		return endpointReady
	}
	if endpointMatchesKnownStack(parent, baseURL) {
		return endpointPortOpenButNotReady
	}
	return endpointUnreachable
}

// isInClusterDNS reports whether the URL points at a Kubernetes in-cluster
// DNS name (anything ending in .svc, .svc.cluster.local, or .cluster.local).
// These resolve only from inside a cluster pod and will never work from
// the user's laptop without a port-forward.
func isInClusterDNS(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return strings.HasSuffix(host, ".svc") ||
		strings.HasSuffix(host, ".svc.cluster.local") ||
		strings.HasSuffix(host, ".cluster.local")
}

// endpointMatchesKnownStack reports whether the URL host is loopback AND a
// bairav-managed docker stack is mapped to its port. The loopback check is
// what stops port-only matches from false-positiving on e.g. a k8s service
// URL that happens to use 8080. Best-effort: a docker query timeout or
// missing daemon returns false silently.
func endpointMatchesKnownStack(parent context.Context, baseURL string) bool {
	if !isLoopbackURL(baseURL) {
		return false
	}
	port := portFromURL(baseURL)
	if port == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	stacks, err := compose.List(ctx)
	if err != nil {
		return false
	}
	for _, s := range stacks {
		if s.HostPort == port {
			return true
		}
	}
	return false
}

// isLoopbackURL reports whether the URL's host is 127.0.0.1, ::1, or localhost.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// portFromURL extracts the TCP port from a "scheme://host:port/…" URL.
// Returns 0 if absent or unparseable.
func portFromURL(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	return 0
}

// waitUntilReady polls the endpoint every 2 s until /models returns 200 or
// the timeout hits. Returns true on ready, false on timeout. Deliberately
// ignores the underlying error shape: as long as the endpoint isn't ready,
// we keep retrying. That covers Linux RST, macOS connection-refused, and
// any future Docker port-forwarding wrinkle uniformly.
func waitUntilReady(parent context.Context, baseURL string, total time.Duration) bool {
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		if probeReady(parent, baseURL) {
			return true
		}
		select {
		case <-parent.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return false
}

// isConnRefused reports whether err looks like a connection-refused failure,
// which deserves a friendlier message than the raw "dial tcp ...: connection
// refused" the net package returns.
func isConnRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

// isNotReady reports whether err looks like "TCP handshake succeeded but the
// server dropped us before responding" — the classic shape when Docker has
// the port mapped but llama-server hasn't started inside the container.
// Matches "connection reset", "EOF", and "unexpected EOF" on the wire.
func isNotReady(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "unexpected EOF") ||
		strings.Contains(msg, ": EOF")
}
