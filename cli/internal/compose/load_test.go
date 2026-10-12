// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"os"
	"reflect"
	"testing"
)

// Render → parseSpec must round-trip, or an upgrade would silently drop a
// user's port, DLP mode or policy mount.
func TestParseSpecRoundTrip(t *testing.T) {
	cases := []Spec{
		{Name: "qwen", Repo: "Qwen/x", HostPort: 18080, ModelsHostDir: "/m"},
		{Name: "qwen", Repo: "Qwen/x", File: "q4.gguf", HostPort: 8088, CtxSize: 4096, ModelsHostDir: "/m", HFToken: "hf_x", Image: "my/serving:1"},
		{Name: "qwen", Repo: "Qwen/x", HostPort: 9090, ModelsHostDir: "/m", DLPMode: DLPModeEnforce, DLPAction: "block", DLPPolicyHostPath: "/abs/p.yaml"},
	}
	for i, want := range cases {
		got, err := parseSpec(want.Name, []byte(Render(want)))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if want.DLPMode == DLPModeEnforce && want.DLPAction == "" {
			want.DLPAction = "redact"
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("case %d:\n got  %+v\n want %+v", i, got, want)
		}
	}
}

func TestParseSpecRejectsForeignFiles(t *testing.T) {
	if _, err := parseSpec("x", []byte("services:\n  web:\n    image: nginx\n")); err == nil {
		t.Error("expected error for a compose file without a serving service")
	}
}

func TestPreviousRoundTrip(t *testing.T) {
	t.Setenv("BAIRAV_COMPOSE_DIR", t.TempDir())
	if _, err := LoadPrevious("qwen"); err == nil {
		t.Error("expected error before any upgrade")
	}
	dir, _ := StackDir("qwen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SavePrevious("qwen", PreviousModel{Repo: "Qwen/old", File: "a.gguf"}); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPrevious("qwen")
	if err != nil || p.Repo != "Qwen/old" || p.File != "a.gguf" {
		t.Errorf("got %+v, %v", p, err)
	}
}
