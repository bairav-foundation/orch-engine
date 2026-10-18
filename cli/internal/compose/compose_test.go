// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"strings"
	"testing"
)

func TestRender_DeterministicEnvOrder(t *testing.T) {
	// Two specs differing only in HFToken (which adds an env key) — env
	// must be alphabetical so the golden lines are stable.
	s := Spec{
		Name:          "qwen-tiny",
		Repo:          "Qwen/Qwen2.5-0.5B-Instruct-GGUF",
		File:          "qwen2.5-0.5b-instruct-q4_k_m.gguf",
		HostPort:      8088,
		CtxSize:       2048,
		ModelsHostDir: "/home/you/.bairav/models",
	}
	got := Render(s)

	// Expected key order (alphabetical): BAIRAV_CTX_SIZE, BAIRAV_FILE,
	// BAIRAV_HOST, BAIRAV_PORT, BAIRAV_REPO. Find each and confirm offsets
	// increase monotonically.
	wantOrder := []string{
		"BAIRAV_CTX_SIZE:",
		"BAIRAV_FILE:",
		"BAIRAV_HOST:",
		"BAIRAV_PORT:",
		"BAIRAV_REPO:",
	}
	last := -1
	for _, key := range wantOrder {
		idx := strings.Index(got, key)
		if idx < 0 {
			t.Fatalf("Render: missing key %q in output:\n%s", key, got)
		}
		if idx <= last {
			t.Errorf("Render: key %q at offset %d came before previous (%d) — env keys not sorted", key, idx, last)
		}
		last = idx
	}
}

func TestRender_PortMappingAndLabels(t *testing.T) {
	s := Spec{
		Name:          "demo",
		Repo:          "TinyLlama/TinyLlama-1.1B-Chat-v1.0",
		HostPort:      9090,
		ModelsHostDir: "/tmp/models",
	}
	got := Render(s)

	mustContain(t, got, `"127.0.0.1:9090:8080"`, "host→container port mapping")
	mustContain(t, got, `"/tmp/models:/models"`, "models bind mount")
	mustContain(t, got, `container_name: bairav-demo`, "container_name uses spec name")
	mustContain(t, got, `bairav.managed: "true"`, "managed label")
	mustContain(t, got, `bairav.name: "demo"`, "name label")
	mustContain(t, got, `bairav.repo: "TinyLlama/TinyLlama-1.1B-Chat-v1.0"`, "repo label")
	mustContain(t, got, `image: bairav/serving:laptop`, "default image")
}

func TestRender_OmitsOptionalEnvs(t *testing.T) {
	s := Spec{
		Name:          "minimal",
		Repo:          "org/repo",
		HostPort:      8080,
		ModelsHostDir: "/tmp/models",
	}
	got := Render(s)

	if strings.Contains(got, "BAIRAV_FILE:") {
		t.Errorf("Render: should not emit BAIRAV_FILE when File is empty:\n%s", got)
	}
	if strings.Contains(got, "BAIRAV_CTX_SIZE:") {
		t.Errorf("Render: should not emit BAIRAV_CTX_SIZE when CtxSize is 0:\n%s", got)
	}
	if strings.Contains(got, "HF_TOKEN:") {
		t.Errorf("Render: should not emit HF_TOKEN when HFToken is empty:\n%s", got)
	}
}

func TestRender_CustomImageOverride(t *testing.T) {
	s := Spec{
		Name:          "custom",
		Repo:          "org/repo",
		Image:         "ghcr.io/myorg/serving:v2",
		HostPort:      8080,
		ModelsHostDir: "/tmp/models",
	}
	got := Render(s)
	mustContain(t, got, "image: ghcr.io/myorg/serving:v2", "custom image override")
}

func TestSpec_Endpoint(t *testing.T) {
	cases := []struct {
		name string
		s    Spec
		want string
	}{
		{"explicit host port", Spec{HostPort: 9090}, "http://127.0.0.1:9090/v1"},
		{"default host port", Spec{HostPort: 0}, "http://127.0.0.1:8080/v1"},
	}
	for _, c := range cases {
		if got := c.s.Endpoint(); got != c.want {
			t.Errorf("%s: Endpoint() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParseLabels(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{"", map[string]string{}},
		{"bairav.managed=true", map[string]string{"bairav.managed": "true"}},
		{
			"bairav.managed=true,bairav.name=qwen,bairav.repo=Qwen/Qwen2.5",
			map[string]string{
				"bairav.managed": "true",
				"bairav.name":    "qwen",
				"bairav.repo":    "Qwen/Qwen2.5",
			},
		},
	}
	for _, c := range cases {
		got := parseLabels(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseLabels(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("parseLabels(%q)[%s] = %q, want %q", c.in, k, got[k], v)
			}
		}
	}
}

func TestExtractHostPort(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"8080/tcp", 0},                  // no host bind
		{"0.0.0.0:8088->8080/tcp", 8088}, // ipv4 only
		{"0.0.0.0:8088->8080/tcp, [::]:8088->8080/tcp", 8088}, // dual-stack
		{"127.0.0.1:9090->8080/tcp", 9090},                    // loopback bind
		{"[::]:7070->8080/tcp, 0.0.0.0:7070->8080/tcp", 7070}, // ipv6 first
	}
	for _, c := range cases {
		if got := extractHostPort(c.in); got != c.want {
			t.Errorf("extractHostPort(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func mustContain(t *testing.T, haystack, needle, label string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: expected %q in output, got:\n%s", label, needle, haystack)
	}
}

func mustNotContain(t *testing.T, haystack, needle, label string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s: did NOT expect %q in output, got:\n%s", label, needle, haystack)
	}
}

// -----------------------------------------------------------------------------
// DLP enforce mode
// -----------------------------------------------------------------------------

func TestRender_OffMode_SingleServiceLegacyShape(t *testing.T) {
	// Empty DLPMode should render identically to today's single-service
	// output — no dlp service, model published on the host.
	s := Spec{
		Name:          "unprotected",
		Repo:          "org/repo",
		HostPort:      8080,
		ModelsHostDir: "/tmp/models",
		// DLPMode intentionally zero-valued
	}
	got := Render(s)

	mustContain(t, got, "  serving:", "serving service header")
	mustNotContain(t, got, "  dlp:", "no dlp service in off mode")
	mustContain(t, got, `"127.0.0.1:8080:8080"`, "model port published in off mode")
	mustNotContain(t, got, "expose:", "off mode uses ports, not expose")
}

func TestRender_EnforceMode_EmitsBothServices(t *testing.T) {
	s := Spec{
		Name:          "protected",
		Repo:          "org/repo",
		HostPort:      9090,
		ModelsHostDir: "/tmp/models",
		DLPMode:       DLPModeEnforce,
		DLPAction:     "block",
	}
	got := Render(s)

	// Both services present.
	mustContain(t, got, "  serving:", "serving service header")
	mustContain(t, got, "  dlp:", "dlp service header")

	// Serving is NOT host-published; uses `expose:` instead of `ports:`.
	// This is the enforcement mechanism — the model has no reachable
	// host port; the compose network is the only way in.
	mustNotContain(t, got, `"127.0.0.1:9090:8080"`, "serving must not publish the host port in enforce mode")
	mustContain(t, got, "    expose:", "serving must use expose in enforce mode")

	// DLP service publishes the host port and forwards to serving:8080.
	mustContain(t, got, `"127.0.0.1:9090:8090"`, "dlp publishes host port → DLP container port")
	mustContain(t, got, `BAIRAV_DLP_UPSTREAM: "http://serving:8080/v1"`, "dlp env forwards to compose service name")
	mustContain(t, got, `BAIRAV_DLP_ACTION: "block"`, "dlp env carries the requested action")
	mustContain(t, got, `BAIRAV_DLP_LISTEN: ":8090"`, "dlp listens on its container port")

	// Both services carry the bairav labels + role for ps parsing.
	mustContain(t, got, `bairav.role: "serving"`, "serving role label")
	mustContain(t, got, `bairav.role: "dlp"`, "dlp role label")

	// The dlp container name is deterministic and matches the ps expectation.
	mustContain(t, got, "container_name: bairav-protected", "serving container name")
	mustContain(t, got, "container_name: bairav-protected-dlp", "dlp container name")

	// depends_on ensures dlp waits for serving in typical compose orchestration.
	mustContain(t, got, "    depends_on:", "dlp depends_on header")
}

func TestRender_EnforceMode_PolicyMountsReadOnly(t *testing.T) {
	// A DLP policy file on the host must appear as a read-only bind
	// mount inside the dlp container and be surfaced via the
	// BAIRAV_DLP_POLICY env var so the entrypoint picks it up.
	s := Spec{
		Name:              "protected-with-policy",
		Repo:              "org/repo",
		HostPort:          8080,
		ModelsHostDir:     "/tmp/models",
		DLPMode:           DLPModeEnforce,
		DLPPolicyHostPath: "/home/you/policies/dlp.yaml",
	}
	got := Render(s)

	mustContain(t, got, `"/home/you/policies/dlp.yaml:/etc/bairav/dlp-policy.yaml:ro"`,
		"policy bind mount must be read-only (:ro)")
	mustContain(t, got, `BAIRAV_DLP_POLICY: "/etc/bairav/dlp-policy.yaml"`,
		"BAIRAV_DLP_POLICY env points at the in-container path")
}

func TestRender_EnforceMode_DefaultActionIsRedact(t *testing.T) {
	// DLPAction empty → default to "redact" in the rendered env var.
	s := Spec{
		Name:          "protected-default-action",
		Repo:          "org/repo",
		HostPort:      8080,
		ModelsHostDir: "/tmp/models",
		DLPMode:       DLPModeEnforce,
		// DLPAction: "",
	}
	got := Render(s)
	mustContain(t, got, `BAIRAV_DLP_ACTION: "redact"`, "empty DLPAction should default to redact")
}

func TestRender_EnforceMode_CustomDLPImageOverride(t *testing.T) {
	s := Spec{
		Name:          "protected-custom-dlp",
		Repo:          "org/repo",
		HostPort:      8080,
		ModelsHostDir: "/tmp/models",
		DLPMode:       DLPModeEnforce,
		DLPImage:      "ghcr.io/myorg/bairav-dlp:hardened",
	}
	got := Render(s)
	mustContain(t, got, "image: ghcr.io/myorg/bairav-dlp:hardened", "custom DLP image override")
}

func TestUseReleaseImages(t *testing.T) {
	saveS, saveD := DefaultImage, DefaultDLPImage
	defer func() { DefaultImage, DefaultDLPImage = saveS, saveD }()

	UseReleaseImages("v0.0.0-dev+abc123")
	if DefaultImage != saveS || DefaultDLPImage != saveD {
		t.Fatalf("dev build changed images: %q %q", DefaultImage, DefaultDLPImage)
	}
	UseReleaseImages("v0.2.0")
	if DefaultImage != "ghcr.io/bairav-foundation/serving:v0.2.0" || DefaultDLPImage != "ghcr.io/bairav-foundation/dlp:v0.2.0" {
		t.Fatalf("release images wrong: %q %q", DefaultImage, DefaultDLPImage)
	}
	t.Setenv("BAIRAV_SERVING_IMAGE", "registry.local/serving:x")
	UseReleaseImages("v0.2.0-rc.1")
	if DefaultImage != "registry.local/serving:x" || DefaultDLPImage != "ghcr.io/bairav-foundation/dlp:v0.2.0-rc.1" {
		t.Fatalf("override wrong: %q %q", DefaultImage, DefaultDLPImage)
	}
}
