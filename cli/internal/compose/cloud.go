// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package compose

// Day 28: cloud Model stacks on the docker target. A cloud stack has one
// service named "serving" (so LoadSpec, `model upgrade`, `model status`
// and Agents' ref resolution treat it like any Model), running the DLP
// image in gateway mode on the usual container port 8080.
//
// The API key never goes into docker-compose.yaml. It is written to
// provider-<provider>.env (0600) in the stack dir and loaded via env_file.
// One file per provider, so a rollback from anthropic to openai picks up
// the openai key again.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LabelProvider marks a cloud Model stack's provider (absent = local).
const LabelProvider = "bairav.provider"

// IsCloud reports whether the stack is served by a cloud provider.
func (s Spec) IsCloud() bool { return s.Provider != "" && s.Provider != "local" }

// ProviderEnvFile is the env_file holding the key for a provider.
func ProviderEnvFile(provider string) string { return "provider-" + provider + ".env" }

// writeProviderKey stores the key for s.Provider in the stack dir. An empty
// key leaves an existing file alone (upgrade/rollback re-render the stack
// without knowing the key) and creates an empty one if missing, so the
// env_file reference never breaks `docker compose up`.
func writeProviderKey(s Spec) error {
	dir, err := StackDir(s.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create stack dir: %w", err)
	}
	path := filepath.Join(dir, ProviderEnvFile(s.Provider))
	if s.APIKey == "" {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		return os.WriteFile(path, nil, 0o600)
	}
	if strings.ContainsAny(s.APIKey, "\r\n") {
		return fmt.Errorf("API key contains a newline; refusing to write it")
	}
	if err := os.WriteFile(path, []byte("BAIRAV_PROVIDER_API_KEY="+s.APIKey+"\n"), 0o600); err != nil {
		return fmt.Errorf("write provider key: %w", err)
	}
	return os.Chmod(path, 0o600) // WriteFile keeps the old mode on an existing file
}

// renderGatewayService writes the cloud stack's single service.
func renderGatewayService(b *strings.Builder, s Spec, hostPort int) {
	image := s.DLPImage
	if image == "" {
		image = DefaultDLPImage
	}
	action := s.DLPAction
	if action == "" {
		action = "redact"
	}
	env := map[string]string{
		"BAIRAV_REPO":          s.Repo, // read back by LoadSpec, like local stacks
		"BAIRAV_DLP_PROVIDER":  s.Provider,
		"BAIRAV_DLP_MODEL":     s.Repo,
		"BAIRAV_DLP_LISTEN":    fmt.Sprintf(":%d", DefaultContainerPort),
		"BAIRAV_DLP_ACTION":    action,
		"BAIRAV_DLP_DIRECTION": "both",
		"BAIRAV_DLP_AUDIT":     "-",
	}
	if s.BaseURL != "" {
		env["BAIRAV_DLP_UPSTREAM"] = s.BaseURL
	}

	fmt.Fprintln(b, "  serving:")
	fmt.Fprintf(b, "    image: %s\n", image)
	fmt.Fprintf(b, "    container_name: bairav-%s\n", s.Name)
	fmt.Fprintln(b, "    restart: unless-stopped")
	fmt.Fprintln(b, "    env_file:")
	fmt.Fprintf(b, "      - %s\n", ProviderEnvFile(s.Provider))
	fmt.Fprintln(b, "    environment:")
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(b, "      %s: %q\n", k, env[k])
	}
	fmt.Fprintln(b, "    ports:")
	fmt.Fprintf(b, "      - %q\n", portMapping(hostPort, DefaultContainerPort))
	fmt.Fprintln(b, "    labels:")
	labels := map[string]string{
		LabelManaged:  "true",
		LabelName:     s.Name,
		LabelRepo:     s.Repo,
		LabelRole:     RoleServing,
		LabelKind:     KindModel,
		LabelProvider: s.Provider,
		LabelEndpoint: fmt.Sprintf("http://%s:%d/v1", ModelNetAlias(s.Name), DefaultContainerPort),
	}
	for _, k := range sortedKeys(labels) {
		fmt.Fprintf(b, "      %s: %q\n", k, labels[k])
	}
	writeServiceNetworks(b, ModelNetAlias(s.Name))
	// Distroless image: no curl. The bairav binary checks its own /healthz.
	fmt.Fprintln(b, "    healthcheck:")
	fmt.Fprintf(b, "      test: [\"CMD\", \"/usr/local/bin/bairav\", \"dlp\", \"ping\", \"http://127.0.0.1:%d/healthz\"]\n", DefaultContainerPort)
	fmt.Fprintln(b, "      interval: 5s")
	fmt.Fprintln(b, "      timeout: 3s")
	fmt.Fprintln(b, "      retries: 12")
}
