// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCloudStackRenderAndParse(t *testing.T) {
	s := Spec{Name: "support", Repo: "gpt-4.1-mini", Provider: "openai", HostPort: 18100, DLPAction: "block", APIKey: "sk-super-secret"}
	y := Render(s)
	if strings.Contains(y, "sk-super-secret") {
		t.Fatal("API key leaked into docker-compose.yaml")
	}
	for _, want := range []string{
		"image: bairav/dlp:laptop", "env_file:", "provider-openai.env",
		`BAIRAV_DLP_PROVIDER: "openai"`, `BAIRAV_DLP_MODEL: "gpt-4.1-mini"`, `"127.0.0.1:18100:8080"`,
		`bairav.provider: "openai"`, `bairav.role: "serving"`, `"dlp", "ping"`,
	} {
		if !strings.Contains(y, want) {
			t.Errorf("rendered stack missing %q:\n%s", want, y)
		}
	}
	if strings.Contains(y, "/models") || strings.Contains(y, "curl") {
		t.Error("cloud stack must not mount models or use curl (distroless image)")
	}

	got, err := parseSpec("support", []byte(y))
	if err != nil {
		t.Fatal(err)
	}
	want := s
	want.APIKey = "" // lives in the env file, not the compose file
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got  %+v\n want %+v", got, want)
	}

	compat := Spec{Name: "vllm", Repo: "meta/llama", Provider: "openai-compatible", BaseURL: "http://gpu:8000/v1", HostPort: 18101, DLPAction: "redact"}
	got, err = parseSpec("vllm", []byte(Render(compat)))
	if err != nil || !reflect.DeepEqual(got, compat) {
		t.Errorf("openai-compatible round trip: %+v, %v", got, err)
	}
}

func TestWriteProviderKey(t *testing.T) {
	t.Setenv("BAIRAV_COMPOSE_DIR", t.TempDir())
	s := Spec{Name: "support", Provider: "anthropic", APIKey: "sk-ant-1"}
	if err := writeProviderKey(s); err != nil {
		t.Fatal(err)
	}
	dir, _ := StackDir("support")
	path := filepath.Join(dir, "provider-anthropic.env")
	b, _ := os.ReadFile(path)
	if string(b) != "BAIRAV_PROVIDER_API_KEY=sk-ant-1\n" {
		t.Errorf("key file = %q", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	// Re-render without a key (upgrade/rollback): keep the existing key.
	s.APIKey = ""
	if err := writeProviderKey(s); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "sk-ant-1") {
		t.Error("empty APIKey must not wipe the stored key")
	}
	if err := writeProviderKey(Spec{Name: "support", Provider: "openai", APIKey: "a\nB=c"}); err == nil {
		t.Error("newline in key must be refused (env-file injection)")
	}
}

// Names become paths, compose project names and YAML values; images and env
// keys are written unquoted. Anything that could escape gets refused.
func TestValidation(t *testing.T) {
	for _, ok := range []string{"qwen", "support-model", "skill-echo", "a_b-1"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../etc", "a/b", "Qwen", "x\n  privileged: true", "-lead", strings.Repeat("a", 64)} {
		if _, err := StackDir(bad); err == nil {
			t.Errorf("StackDir(%q) should be refused", bad)
		}
	}
	if err := validateRender("ghcr.io/org/img@sha256:abc", map[string]string{"FOO_1": "x y"}); err != nil {
		t.Errorf("valid image/env refused: %v", err)
	}
	if err := validateRender("nginx\n    privileged: true", nil); err == nil {
		t.Error("image with newline must be refused")
	}
	if err := validateRender("nginx", map[string]string{"A: b\n  c": "1"}); err == nil {
		t.Error("env key injection must be refused")
	}
}
