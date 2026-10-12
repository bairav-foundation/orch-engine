// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func cloudCmd(args ...string) *cobra.Command {
	c := &cobra.Command{}
	addCloudFlags(c)
	_ = c.Flags().Parse(args)
	return c
}

func TestReadCloudOpts(t *testing.T) {
	o, err := readCloudOpts(cloudCmd())
	if err != nil || o.isCloud() || o.changed || o.provider != "local" {
		t.Errorf("no flags → local, unchanged: %+v %v", o, err)
	}
	o, err = readCloudOpts(cloudCmd("--provider", "Anthropic", "--api-key-secret", "ant"))
	if err != nil || o.provider != "anthropic" || o.secretName != "ant" || o.secretKey != "api-key" || !o.changed {
		t.Errorf("anthropic: %+v %v", o, err)
	}
	for _, bad := range [][]string{
		{"--provider", "gemini"},
		{"--provider", "openai-compatible"},           // needs --base-url
		{"--base-url", "http://x/v1"},                 // needs a cloud provider
		{"--provider", "local", "--api-key-env", "K"}, // ditto
	} {
		if _, err := readCloudOpts(cloudCmd(bad...)); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}
}

func TestProviderKeyPrecedence(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("BAIRAV_PROVIDER_API_KEY", "sk-generic")
	t.Setenv("MY_KEY", "sk-mine")
	if k, from := providerKey(cloudOpts{provider: "openai"}); k != "sk-openai" || from != "OPENAI_API_KEY" {
		t.Errorf("openai: %s from %s", k, from)
	}
	if k, _ := providerKey(cloudOpts{provider: "anthropic"}); k != "sk-generic" {
		t.Errorf("anthropic falls back to BAIRAV_PROVIDER_API_KEY, got %s", k)
	}
	if k, from := providerKey(cloudOpts{provider: "openai", keyEnv: "MY_KEY"}); k != "sk-mine" || from != "MY_KEY" {
		t.Errorf("--api-key-env wins: %s from %s", k, from)
	}
}

func TestApplyCloudK8sWithoutClientCalls(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("BAIRAV_PROVIDER_API_KEY", "")
	var spec orchv1alpha1.ModelSpec
	// Existing Secret named on the command line: no client call needed.
	o := cloudOpts{provider: "anthropic", secretName: "ant", secretKey: "k", changed: true}
	if err := applyCloudK8s(context.Background(), nil, "ns", "m", o, &spec, io.Discard); err != nil {
		t.Fatal(err)
	}
	if spec.Provider != "anthropic" || spec.Cloud.APIKeySecretRef.Name != "ant" || spec.Cloud.APIKeySecretRef.Key != "k" {
		t.Errorf("spec: %+v", spec)
	}
	// No secret, no env key → clear error naming the env var.
	spec = orchv1alpha1.ModelSpec{}
	err := applyCloudK8s(context.Background(), nil, "ns", "m", cloudOpts{provider: "anthropic"}, &spec, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("want missing-key error, got %v", err)
	}
	// Back to local clears the cloud block.
	spec = orchv1alpha1.ModelSpec{Provider: "openai", Cloud: &orchv1alpha1.ModelCloudSpec{}}
	if err := applyCloudK8s(context.Background(), nil, "ns", "m", cloudOpts{provider: "local"}, &spec, io.Discard); err != nil || spec.Cloud != nil || spec.Provider != "local" {
		t.Errorf("local: %+v %v", spec, err)
	}
}

func TestModelRef(t *testing.T) {
	gpt := refOf(orchv1alpha1.ModelSpec{Repo: "gpt-4.1-mini", Provider: "openai", Cloud: &orchv1alpha1.ModelCloudSpec{}})
	qwen := refOf(orchv1alpha1.ModelSpec{Repo: "Qwen/Qwen2.5-1.5B-Instruct-GGUF"})
	if gpt.label() != "openai:gpt-4.1-mini" || qwen.label() != "Qwen/Qwen2.5-1.5B-Instruct-GGUF" || qwen.Provider != "local" {
		t.Errorf("labels: %q %q", gpt.label(), qwen.label())
	}
	if gpt.same(qwen) || !qwen.same(modelRef{Repo: "Qwen/Qwen2.5-1.5B-Instruct-GGUF"}) {
		t.Error("same() wrong")
	}
	// The rollback annotation must round-trip provider + cloud.
	var back modelRef
	if err := json.Unmarshal([]byte(gpt.encode()), &back); err != nil || !back.same(gpt) || back.Cloud == nil {
		t.Errorf("encode round trip: %+v %v", back, err)
	}
}
