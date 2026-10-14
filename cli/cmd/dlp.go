// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/bairav-foundation/orch-engine/cli/internal/dlp"
	"github.com/bairav-foundation/orch-engine/cli/internal/dlp/packs"
)

// dlpCmd groups `bairav dlp <subcommand>` — serve and test.
//
// Wiring status:
//
//	serve — HTTP proxy in front of any OpenAI endpoint
//	test  — one-shot detector dry-run
var dlpCmd = &cobra.Command{
	Use:   "dlp",
	Short: "Prompt DLP — detect, redact, or block PII/secrets before they leave the perimeter",
	Long: `BAIRAVS DLP is an HTTP proxy that sits in front of an OpenAI-compatible
model endpoint and inspects every prompt for PII and secrets. By default it
redacts hits to [REDACTED:LABEL] before forwarding; --action block rejects
the request with HTTP 403; --action audit_only forwards unchanged but logs
every hit (useful for measuring exposure before turning enforcement on).

Built-in detectors: EMAIL, SSN, PAN (credit card with Luhn), PHONE_US, AWS_ACCESS_KEY.`,
}

func init() {
	dlpCmd.AddCommand(dlpServeCmd)
	dlpCmd.AddCommand(dlpTestCmd)
	dlpCmd.AddCommand(dlpPacksCmd)
	dlpCmd.AddCommand(dlpChecksumCmd)
	dlpPacksCmd.AddCommand(dlpPacksListCmd)

	// --policy is a StringSlice on both subcommands so users can compose
	// packs: `--policy india.yaml --policy corp.yaml`. Rules from every
	// file stack on top of the built-ins; a label defined in two files
	// fails at load time (see LoadPolicyFiles).
	dlpTestCmd.Flags().StringSlice("policy", nil,
		"YAML policy file(s) with custom detection rules (repeatable; stacks on top of built-ins + packs)")
	// --pack is StringSlice with default ["global-safe"]. global-safe is
	// on by default because it only contains checksum-anchored rules —
	// near-zero false-positive rate regardless of caller geography, so
	// multinationals get real Aadhaar/NHS/etc. detection without config.
	// Pass --pack explicitly to override the default (e.g. --pack india
	// = only india; --pack none = disable all built-in packs).
	dlpTestCmd.Flags().StringSlice("pack", []string{"global-safe"},
		"built-in pack(s) to enable (default: global-safe; use \"none\" to disable, or list explicitly: india,uk)")

	dlpServeCmd.Flags().String("upstream", "http://127.0.0.1:8080/v1",
		"OpenAI-compatible upstream the proxy forwards to")
	dlpServeCmd.Flags().String("listen", "127.0.0.1:8090",
		"address to listen on (default loopback only; containers set BAIRAV_DLP_LISTEN=:8090)")
	dlpServeCmd.Flags().String("action", "redact",
		"default policy on hit when a rule doesn't override: redact | block | audit_only")
	dlpServeCmd.Flags().String("audit", "",
		"audit log path (default: ~/.bairav/dlp/audit-YYYY-MM-DD.jsonl)")
	dlpServeCmd.Flags().StringSlice("policy", nil,
		"YAML policy file(s) with custom detection rules (repeatable; stacks on top of built-ins + packs)")
	dlpServeCmd.Flags().StringSlice("pack", []string{"global-safe"},
		"built-in pack(s) to enable (default: global-safe; use \"none\" to disable, or list explicitly: india,uk)")
	dlpServeCmd.Flags().String("provider", "",
		"gateway mode: front a cloud provider (openai | anthropic | openai-compatible). The API key is read from $BAIRAV_PROVIDER_API_KEY, never a flag")
	dlpServeCmd.Flags().Bool("audit-required", false,
		"treat a failed audit write as fatal: report unhealthy and refuse requests (env BAIRAV_DLP_AUDIT_REQUIRED=true)")
	dlpServeCmd.Flags().String("audit-matches", dlp.AuditMatchesMasked,
		"how matched text appears in the audit log: masked (default) | raw | none")
	dlpServeCmd.Flags().String("model", "",
		"gateway mode: pin every request to this provider model id (e.g. gpt-4.1-mini)")
	dlpServeCmd.Flags().String("direction", "request",
		"which side to inspect: request | response | both (default: request)")
	dlpServeCmd.Flags().Bool("litellm-guardrail", false,
		"serve the LiteLLM Generic Guardrail API (POST "+dlp.LiteLLMGuardrailPath+") instead of proxying; "+
			"--upstream/--provider/--model are ignored (env BAIRAV_DLP_MODE=litellm-guardrail)")
	dlpServeCmd.Flags().Bool("mcp", false,
		"proxy an MCP server instead of a model: inspect tool arguments and tool results on any path, "+
			"over JSON or SSE; --upstream is the MCP server's base URL, direction defaults to both (env BAIRAV_DLP_MODE=mcp)")
	dlpServeCmd.Flags().String("upstream-health", "",
		"MCP mode: path on the upstream that /healthz also checks, e.g. /health (env BAIRAV_DLP_UPSTREAM_HEALTH)")
}

// -------------------------------------------------------------------------
// dlp serve
// -------------------------------------------------------------------------

var dlpServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the DLP HTTP proxy in front of a model endpoint",
	Long: `Start an HTTP proxy that inspects /v1/chat/completions, /v1/completions,
and /v1/embeddings before forwarding to the upstream model. Stream responses
are passed through unchanged. Every decision is written to the audit log.

Typical flow:
  # Terminal 1: deploy a model (default docker target)
  bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF

  # Terminal 2: stand up the proxy in front of it
  bairav dlp serve --upstream http://127.0.0.1:8080/v1 --listen :8090

  # Terminal 3: chat through the proxy instead of directly
  bairav chat --endpoint http://127.0.0.1:8090/v1

Anything sensitive in the prompt is redacted before the model ever sees it.`,
	RunE: runDLPServe,
}

func runDLPServe(cmd *cobra.Command, _ []string) error {
	// Flag values fall back to BAIRAV_DLP_* environment variables when the
	// user didn't pass the flag explicitly. This is what lets bairav/dlp
	// (the container image) run with zero args — the compose stack sets
	// env vars, this code translates them into effective flag values.
	// Explicit CLI flags always win over env (Changed() reports whether
	// the flag was set on the command line).
	envDefaults := map[string]string{
		"upstream":  "BAIRAV_DLP_UPSTREAM",
		"listen":    "BAIRAV_DLP_LISTEN",
		"action":    "BAIRAV_DLP_ACTION",
		"audit":     "BAIRAV_DLP_AUDIT",
		"direction": "BAIRAV_DLP_DIRECTION",
		"provider":  "BAIRAV_DLP_PROVIDER",
		"model":     "BAIRAV_DLP_MODEL",
		// BAIRAV_DLP_AUDIT_MATCHES=raw restores verbatim matches (SOC review).
		"audit-matches":   "BAIRAV_DLP_AUDIT_MATCHES",
		"upstream-health": "BAIRAV_DLP_UPSTREAM_HEALTH",
	}
	for flagName, envKey := range envDefaults {
		if !cmd.Flags().Changed(flagName) {
			if v := os.Getenv(envKey); v != "" {
				_ = cmd.Flags().Set(flagName, v)
			}
		}
	}
	// --policy is a StringSlice; env fallback uses a comma-separated list
	// (BAIRAV_DLP_POLICY=/etc/bairav/india.yaml,/etc/bairav/corp.yaml) so
	// the container can be handed multiple packs via one env var. pflag's
	// StringSlice Set() splits on comma by default.
	if !cmd.Flags().Changed("policy") {
		if v := os.Getenv("BAIRAV_DLP_POLICY"); v != "" {
			_ = cmd.Flags().Set("policy", v)
		}
	}
	// --pack env fallback. The K8s sidecar passes the
	// DLPPolicy's packs as BAIRAV_DLP_PACK=global-safe,india. Without this
	// the sidecar silently fell back to the global-safe default and ignored
	// the policy's pack list. The first Set() on an unchanged pflag
	// StringSlice replaces the default (it only appends on later Sets),
	// so this overrides global-safe rather than stacking on it.
	if !cmd.Flags().Changed("pack") {
		if v := os.Getenv("BAIRAV_DLP_PACK"); v != "" {
			_ = cmd.Flags().Set("pack", v)
		}
	}

	listen, _ := cmd.Flags().GetString("listen")
	actionRaw, _ := cmd.Flags().GetString("action")
	auditPath, _ := cmd.Flags().GetString("audit")
	policyPaths, _ := cmd.Flags().GetStringSlice("policy")
	directionRaw, _ := cmd.Flags().GetString("direction")
	provider, _ := cmd.Flags().GetString("provider")
	pinnedModel, _ := cmd.Flags().GetString("model")
	auditMatches, _ := cmd.Flags().GetString("audit-matches")
	auditRequired, _ := cmd.Flags().GetBool("audit-required")
	if !cmd.Flags().Changed("audit-required") && os.Getenv("BAIRAV_DLP_AUDIT_REQUIRED") == "true" {
		auditRequired = true
	}
	// Caller tokens: env only (a flag would show up in `ps`). Comma-separated
	// in BAIRAV_DLP_CLIENT_TOKENS, or one per line in the file named by
	// BAIRAV_DLP_CLIENT_TOKENS_FILE (a mounted Secret).
	clientTokens, err := loadClientTokens()
	if err != nil {
		return err
	}
	switch auditMatches {
	case dlp.AuditMatchesMasked, dlp.AuditMatchesRaw, dlp.AuditMatchesNone:
	default:
		return fmt.Errorf("unknown --audit-matches %q (use masked, raw or none)", auditMatches)
	}
	apiKey := os.Getenv("BAIRAV_PROVIDER_API_KEY")
	if provider != "" {
		if !dlp.ValidProvider(provider) {
			return fmt.Errorf("unknown --provider %q (use openai, anthropic or openai-compatible)", provider)
		}
		// The localhost default upstream makes no sense for a provider:
		// use the provider's own API unless one was given explicitly.
		if !cmd.Flags().Changed("upstream") && os.Getenv("BAIRAV_DLP_UPSTREAM") == "" {
			if def := dlp.ProviderBaseURL(provider); def != "" {
				_ = cmd.Flags().Set("upstream", def)
			} else {
				return fmt.Errorf("--provider %s needs --upstream (or BAIRAV_DLP_UPSTREAM) set to the API base URL", provider)
			}
		}
		if apiKey == "" && provider != dlp.ProviderOpenAICompatible {
			return fmt.Errorf("--provider %s needs the API key in $BAIRAV_PROVIDER_API_KEY", provider)
		}
	}

	action, err := parseDLPAction(actionRaw)
	if err != nil {
		return err
	}
	mcpMode, _ := cmd.Flags().GetBool("mcp")
	if !cmd.Flags().Changed("mcp") && os.Getenv("BAIRAV_DLP_MODE") == "mcp" {
		mcpMode = true
	}
	if mcpMode && provider != "" {
		return fmt.Errorf("--mcp and --provider don't mix: --upstream is the MCP server")
	}
	// Tool results flow straight into the model's context, so MCP mode
	// inspects both sides unless told otherwise.
	if mcpMode && !cmd.Flags().Changed("direction") && os.Getenv("BAIRAV_DLP_DIRECTION") == "" {
		directionRaw = string(dlp.DirectionBoth)
	}
	direction, err := parseDLPDirection(directionRaw)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()

	// Audit sink resolution. Three modes:
	//   ""         → default to ~/.bairav/dlp/audit-YYYY-MM-DD.jsonl (host CLI use)
	//   "-", "stdout" → write JSONL to os.Stdout (container-friendly; `docker
	//                logs -f bairav-<name>-dlp | jq` shows the audit stream)
	//   <path>     → write to that file (must be a writable path)
	//
	// The container image sets BAIRAV_DLP_AUDIT=- so the DLP sidecar in
	// enforce mode logs audit to stdout by default — no filesystem
	// permissions to worry about, and users see the stream via docker logs.
	var audit dlp.AuditWriter
	switch auditPath {
	case "-", "stdout":
		audit = dlp.NewWriterAuditWriter(os.Stdout)
	case "":
		defaultPath, derr := defaultAuditPath()
		if derr != nil {
			return derr
		}
		auditPath = defaultPath
		fa, ferr := dlp.NewFileAuditWriter(auditPath)
		if ferr != nil {
			return fmt.Errorf("open audit log: %w", ferr)
		}
		defer func() { _ = fa.Close() }()
		audit = fa
	default:
		fa, ferr := dlp.NewFileAuditWriter(auditPath)
		if ferr != nil {
			return fmt.Errorf("open audit log: %w", ferr)
		}
		defer func() { _ = fa.Close() }()
		audit = fa
	}

	// Detector set = built-ins + enabled packs + user --policy files.
	// Order: built-ins are always on; packs stack next; policy files last.
	// LoadMany and LoadPolicyFiles both reject cross-source label
	// collisions with a clear error naming both sources.
	detectors := dlp.DefaultDetectors()
	var policy *dlp.Policy

	packNames, _ := cmd.Flags().GetStringSlice("pack")
	packNames = normalisePackList(packNames)
	if len(packNames) > 0 {
		packDets, packPolicy, perr := packs.LoadMany(packNames)
		if perr != nil {
			return perr
		}
		detectors = append(detectors, packDets...)
		policy = packPolicy
	}

	if len(policyPaths) > 0 {
		policyDets, p, perr := dlp.LoadPolicyFiles(policyPaths)
		if perr != nil {
			return perr
		}
		if err := checkPackVsPolicyCollision(policy, p); err != nil {
			return err
		}
		detectors = append(detectors, policyDets...)
		if policy == nil {
			policy = p
		} else if p != nil {
			policy.Rules = append(policy.Rules, p.Rules...)
		}
	}

	guardrailMode, _ := cmd.Flags().GetBool("litellm-guardrail")
	if !cmd.Flags().Changed("litellm-guardrail") && os.Getenv("BAIRAV_DLP_MODE") == "litellm-guardrail" {
		guardrailMode = true
	}
	if guardrailMode && mcpMode {
		return fmt.Errorf("--litellm-guardrail and --mcp are different modes; pick one")
	}
	if guardrailMode && provider != "" {
		return fmt.Errorf("--litellm-guardrail and --provider don't mix: in guardrail mode LiteLLM calls the provider, not bairav")
	}

	upstream, _ := cmd.Flags().GetString("upstream")
	cfg := dlp.Config{
		Provider:      provider,
		APIKey:        apiKey,
		AuditMatches:  auditMatches,
		AuditRequired: auditRequired,
		ClientTokens:  clientTokens,
		Model:         pinnedModel,
		Upstream:      upstream,
		Action:        action,
		Audit:         audit,
		Detectors:     detectors,
		Direction:     direction,
	}
	var handler http.Handler
	switch {
	case guardrailMode:
		handler = dlp.NewGuardrail(cfg)
	case mcpMode:
		upstreamHealth, _ := cmd.Flags().GetString("upstream-health")
		opt := dlp.MCPOptions{UpstreamHealth: upstreamHealth}
		// Remote MCP servers: the credential comes from the environment
		// (a mounted Secret on k8s), never a flag.
		if tok := strings.TrimSpace(os.Getenv("BAIRAV_MCP_UPSTREAM_TOKEN")); tok != "" {
			opt.UpstreamHeaders = http.Header{"Authorization": {"Bearer " + tok}}
		}
		mp, perr := dlp.NewMCPProxy(cfg, opt)
		if perr != nil {
			return perr
		}
		handler = mp
	default:
		proxy, perr := dlp.NewProxy(cfg)
		if perr != nil {
			return perr
		}
		handler = proxy
	}

	// No WriteTimeout: streamed completions legitimately run for minutes.
	// ReadHeaderTimeout stops slowloris; IdleTimeout reaps idle keep-alives.
	srv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown on Ctrl-C / SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	}()

	// Version first so `docker logs` / `kubectl logs -c dlp-<model>` tell you
	// which build is running (stale-image bugs were a recurring time sink).
	fmt.Fprintf(out, "bairav dlp serve %s\n", version)
	fmt.Fprintf(out, "  listening: http://%s\n", listenForDisplay(listen))
	if guardrailMode {
		fmt.Fprintf(out, "  mode:      LiteLLM guardrail (POST %s); nothing is forwarded\n", dlp.LiteLLMGuardrailPath)
	} else if mcpMode {
		fmt.Fprintf(out, "  mode:      MCP (tool arguments and results, JSON and SSE)\n")
		fmt.Fprintf(out, "  upstream:  %s\n", upstream)
	} else {
		fmt.Fprintf(out, "  upstream:  %s\n", upstream)
	}
	if provider != "" {
		keyState := "not set"
		if apiKey != "" {
			keyState = "set (from env, never logged)"
		}
		fmt.Fprintf(out, "  gateway:   provider=%s model=%s key=%s\n", provider, orDefault(pinnedModel, "(caller's choice)"), keyState)
		fmt.Fprintln(out, "             inference paths only; caller credentials stripped; unparseable bodies refused")
	}
	fmt.Fprintf(out, "  action:    %s (default; per-rule action overrides win)\n", action)
	fmt.Fprintf(out, "  direction: %s\n", direction)
	// Show "stdout" instead of the literal "-" when audit goes to os.Stdout
	// so the banner is self-explanatory to someone reading `docker logs`.
	auditDisplay := auditPath
	if auditPath == "-" {
		auditDisplay = "stdout (tail with `docker logs -f`)"
	}
	fmt.Fprintf(out, "  audit:     %s (matches: %s, required: %v)\n", auditDisplay, auditMatches, auditRequired)
	if len(clientTokens) > 0 {
		fmt.Fprintf(out, "  callers:   %d token(s) accepted; requests without one get 401\n", len(clientTokens))
	} else if provider != "" {
		fmt.Fprintln(out, "  callers:   ⚠ no client tokens: anything that can reach this port can spend the provider key")
	}
	fmt.Fprintln(out, "Built-in detectors:", strings.Join(detectorNames(dlp.DefaultDetectors()), ", "))
	if len(packNames) > 0 {
		fmt.Fprintf(out, "Packs enabled:      %s\n", strings.Join(packNames, ", "))
	} else {
		fmt.Fprintln(out, "Packs enabled:      (none — built-ins only)")
	}
	if len(policyPaths) > 0 {
		fmt.Fprintf(out, "Policy files:       %s\n", strings.Join(policyPaths, ", "))
	}
	if policy != nil {
		fmt.Fprintln(out, "Custom rules:      ", strings.Join(dlp.SortedRuleLabels(policy), ", "))
	}
	fmt.Fprintln(out)
	if guardrailMode {
		fmt.Fprintln(out, "Add to LiteLLM config.yaml (litellm_settings.guardrails):")
		fmt.Fprintln(out, "  - guardrail_name: bairav-dlp")
		fmt.Fprintln(out, "    litellm_params:")
		fmt.Fprintln(out, "      guardrail: generic_guardrail_api")
		fmt.Fprintln(out, "      mode: pre_call   # add a second entry with post_call to check responses too")
		fmt.Fprintf(out, "      api_base: http://%s\n", listenForDisplay(listen))
	} else {
		fmt.Fprintln(out, "Point bairav chat at the proxy:")
		fmt.Fprintf(out, "  bairav chat --endpoint http://%s/v1\n", listenForDisplay(listen))
	}
	fmt.Fprintln(out)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: %w", err)
	}
	fmt.Fprintln(out, "dlp serve: shut down cleanly")
	return nil
}

// -------------------------------------------------------------------------
// dlp test
// -------------------------------------------------------------------------

var dlpTestCmd = &cobra.Command{
	Use:   "test [text]",
	Short: "Run the DLP detectors against a one-shot string",
	Long: `Quick feedback loop for tuning policies. Reads from the argument, or
stdin if no argument is given. Prints every hit (label, span, matched text)
and the redacted version.

Example:
  bairav dlp test "my SSN is 123-45-6789"
  cat suspicious.txt | bairav dlp test`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDLPTest,
}

func runDLPTest(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()

	var input string
	switch {
	case len(args) == 1:
		input = args[0]
	default:
		// Read everything from stdin.
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		stdin := cmd.InOrStdin()
		for {
			n, err := stdin.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				break
			}
		}
		input = string(buf)
	}
	if strings.TrimSpace(input) == "" {
		return fmt.Errorf("dlp test: no input (pass an argument or pipe via stdin)")
	}

	// Detector set = built-ins + enabled packs + user --policy files.
	// Same layering as dlp serve; keeps `dlp test` behaviourally
	// identical to what the serving proxy would see for the same
	// prompt, which is the whole point of `dlp test`.
	detectors := dlp.DefaultDetectors()
	packNames, _ := cmd.Flags().GetStringSlice("pack")
	packNames = normalisePackList(packNames)
	if len(packNames) > 0 {
		packDets, _, err := packs.LoadMany(packNames)
		if err != nil {
			return err
		}
		detectors = append(detectors, packDets...)
	}
	if policyPaths, _ := cmd.Flags().GetStringSlice("policy"); len(policyPaths) > 0 {
		extra, _, err := dlp.LoadPolicyFiles(policyPaths)
		if err != nil {
			return err
		}
		detectors = append(detectors, extra...)
	}

	hits := dlp.Scan(input, detectors)
	fmt.Fprintf(out, "Hits: %d\n", len(hits))
	if len(hits) > 0 {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  LABEL\tSTART\tEND\tMATCH")
		for _, h := range hits {
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\n", h.Label, h.Start, h.End, h.Match)
		}
		_ = tw.Flush()
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Redacted:")
		fmt.Fprintln(out, dlp.Redact(input, hits))
	} else {
		fmt.Fprintln(out, "(clean — nothing matched)")
	}
	return nil
}

// -------------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------------

func parseDLPAction(s string) (dlp.Action, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "redact":
		return dlp.ActionRedact, nil
	case "block":
		return dlp.ActionBlock, nil
	case "audit_only", "audit-only", "audit":
		return dlp.ActionAuditOnly, nil
	}
	return "", fmt.Errorf("unknown --action %q (use redact | block | audit_only)", s)
}

// parseDLPDirection accepts "request", "response", or "both" (case-
// insensitive; empty defaults to "request" for backwards compat with
// request-only behaviour, where the proxy always inspected request only).
func parseDLPDirection(s string) (dlp.Direction, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "request", "req":
		return dlp.DirectionRequest, nil
	case "response", "resp":
		return dlp.DirectionResponse, nil
	case "both":
		return dlp.DirectionBoth, nil
	}
	return "", fmt.Errorf("unknown --direction %q (use request | response | both)", s)
}

func defaultAuditPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	name := fmt.Sprintf("audit-%s.jsonl", time.Now().Format("2006-01-02"))
	return filepath.Join(home, ".bairav", "dlp", name), nil
}

// listenForDisplay turns ":8090" into "127.0.0.1:8090" for printing.
// Pure cosmetic — the server actually binds to all interfaces.
func listenForDisplay(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "127.0.0.1" + listen
	}
	return listen
}

// -------------------------------------------------------------------------
// dlp packs list — enumerate built-in bundles
// -------------------------------------------------------------------------

var dlpPacksCmd = &cobra.Command{
	Use:   "packs",
	Short: "Manage built-in DLP rule packs",
}

var dlpPacksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every built-in rule pack shipped with the CLI",
	Long: `Lists the packs baked into the binary. global-safe is on by default
(checksum-anchored rules from every jurisdiction we support — safe FP rate
regardless of caller geography). Regional packs are opt-in via --pack.`,
	RunE: runDLPPacksList,
}

func runDLPPacksList(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	names := packs.PackNames()
	if len(names) == 0 {
		fmt.Fprintln(out, "(no packs embedded — this shouldn't happen; report a bug)")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PACK\tRULES\tDEFAULT")
	for _, name := range names {
		p, err := packs.Load(name)
		if err != nil {
			// Should never happen for embedded packs — surface anyway.
			fmt.Fprintf(tw, "%s\t(error: %v)\t\n", name, err)
			continue
		}
		defaultMark := ""
		if name == "global-safe" {
			defaultMark = "✓"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			name,
			strings.Join(dlp.SortedRuleLabels(p.Policy), ", "),
			defaultMark)
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Enable a pack with:  bairav dlp serve --pack india,uk")
	fmt.Fprintln(out, "Disable defaults:    bairav dlp serve --pack none")
	return nil
}

// -------------------------------------------------------------------------
// dlp checksum — compute check digits for tuning
// -------------------------------------------------------------------------

var dlpChecksumCmd = &cobra.Command{
	Use:   "checksum <algo> <prefix>",
	Short: "Compute a checksum digit for a numeric prefix (tuning helper)",
	Long: `Compute the check digit for a numeric prefix under a given algorithm.
Useful when authoring a policy rule with checksum: verhoeff|luhn|mod11 and
you want a valid test vector without doing the math by hand.

Examples:
  bairav dlp checksum verhoeff 12345678901    # Aadhaar-shape (11-digit prefix → 12-digit valid)
  bairav dlp checksum mod11 401023213         # UK NHS-shape (9-digit prefix → 10-digit valid)
  bairav dlp checksum luhn 424242424242424    # Credit-card-shape (15-digit prefix → 16-digit valid)`,
	Args: cobra.ExactArgs(2),
	RunE: runDLPChecksum,
}

func runDLPChecksum(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	algo := strings.ToLower(strings.TrimSpace(args[0]))
	prefix := strings.TrimSpace(args[1])

	// Sanity: prefix must be all digits. Anything else is user error and
	// the algorithm-specific error would be less clear than saying it here.
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if c < '0' || c > '9' {
			return fmt.Errorf("prefix must be digits only, got %q", prefix)
		}
	}

	var check int
	switch algo {
	case "verhoeff":
		check = dlp.VerhoeffCheckDigit(prefix)
	case "luhn", "mod10":
		check = dlp.LuhnCheckDigit(prefix)
	case "mod11":
		check = dlp.Mod11CheckDigit(prefix)
	default:
		return fmt.Errorf("unknown checksum %q (available: verhoeff, luhn, mod10, mod11)", algo)
	}
	if check < 0 {
		return fmt.Errorf("%s: cannot compute a valid check digit for prefix %q "+
			"(some prefixes are inherently unusable — try a different one)", algo, prefix)
	}
	fmt.Fprintf(out, "Prefix:      %s\n", prefix)
	fmt.Fprintf(out, "Algorithm:   %s\n", algo)
	fmt.Fprintf(out, "Check digit: %d\n", check)
	fmt.Fprintf(out, "Full valid:  %s%d\n", prefix, check)
	return nil
}

func detectorNames(ds []dlp.Detector) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Name())
	}
	return out
}

// normalisePackList handles the special "none" sentinel: if any element
// of names is exactly "none" (case-insensitive), the whole list is
// treated as empty. This is what `--pack none` looks like to the runtime
// — "explicitly disable the default global-safe" without having to
// invent a separate --no-pack flag.
//
// Also trims whitespace on each entry so `--pack "india, uk"` behaves
// the same as `--pack india,uk`.
func normalisePackList(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if strings.EqualFold(n, "none") {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// checkPackVsPolicyCollision surfaces cross-source label collisions
// between a pack-loaded policy and a file-loaded policy. Same shape of
// error message as the intra-source collisions so users see a
// consistent story regardless of which pair collided.
func checkPackVsPolicyCollision(packPolicy, filePolicy *dlp.Policy) error {
	if packPolicy == nil || filePolicy == nil {
		return nil
	}
	packLabels := map[string]bool{}
	for _, r := range packPolicy.Rules {
		packLabels[r.Label] = true
	}
	for _, r := range filePolicy.Rules {
		if packLabels[r.Label] {
			return fmt.Errorf(
				"dlp: label %q is defined in both a --pack and a --policy file — remove one or rename",
				r.Label)
		}
	}
	return nil
}

// loadClientTokens reads accepted caller tokens from the environment.
func loadClientTokens() ([]string, error) {
	var raw []string
	if v := os.Getenv("BAIRAV_DLP_CLIENT_TOKENS"); v != "" {
		raw = append(raw, strings.Split(v, ",")...)
	}
	if f := os.Getenv("BAIRAV_DLP_CLIENT_TOKENS_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read BAIRAV_DLP_CLIENT_TOKENS_FILE: %w", err)
		}
		raw = append(raw, strings.Split(string(b), "\n")...)
	}
	var out []string
	for _, t := range raw {
		if t = strings.TrimSpace(t); t != "" {
			if len(t) < 16 {
				return nil, fmt.Errorf("client token too short (%d chars); use at least 16 random characters", len(t))
			}
			out = append(out, t)
		}
	}
	return out, nil
}
