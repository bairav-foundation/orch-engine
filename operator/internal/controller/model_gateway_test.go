// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func cloudModel(provider string, cloud *orchv1alpha1.ModelCloudSpec) *orchv1alpha1.Model {
	m := &orchv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team-a"}}
	m.Spec.Repo = "gpt-4.1-mini"
	m.Spec.Provider = provider
	m.Spec.Image = "bairav/serving:laptop" // CRD default lands here even for cloud models
	m.Spec.Cloud = cloud
	return m
}

func envMap(c corev1.Container) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

func TestValidateModel(t *testing.T) {
	key := &orchv1alpha1.SecretKeyRef{Name: "k", Key: "api-key"}
	cases := []struct {
		m    *orchv1alpha1.Model
		want string // substring of the reason; "" = valid
	}{
		{cloudModel("", nil), ""},
		{cloudModel("local", nil), ""},
		{cloudModel("openai", nil), "apiKeySecretRef"},
		{cloudModel("anthropic", &orchv1alpha1.ModelCloudSpec{}), "kubectl create secret generic support-provider-key -n team-a"},
		{cloudModel("anthropic", &orchv1alpha1.ModelCloudSpec{APIKeySecretRef: key}), ""},
		{cloudModel("openai-compatible", nil), "baseURL"},
		{cloudModel("openai-compatible", &orchv1alpha1.ModelCloudSpec{BaseURL: "http://vllm:8000/v1"}), ""}, // keyless OK
	}
	for i, c := range cases {
		got := validateModel(c.m)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("case %d (%s): got %q, want %q", i, c.m.Spec.Provider, got, c.want)
		}
	}
}

func TestBuildDeployment_CloudGateway(t *testing.T) {
	r := &ModelReconciler{}
	m := cloudModel("anthropic", &orchv1alpha1.ModelCloudSpec{
		APIKeySecretRef: &orchv1alpha1.SecretKeyRef{Name: "ant", Key: "api-key"},
		DLPPacks:        []string{"global-safe", "india"},
	})
	m.Spec.Repo = "claude-sonnet-4-5"
	d := r.buildDeployment(m)
	pod := d.Spec.Template.Spec
	if len(pod.Containers) != 1 || len(pod.Volumes) != 0 {
		t.Fatalf("gateway pod should be one container, no model volume: %+v", pod)
	}
	c := pod.Containers[0]
	if c.Image != DLPSidecarImage {
		t.Errorf("image = %q, want the DLP image (not the CRD's serving default)", c.Image)
	}
	env := envMap(c)
	for k, want := range map[string]string{
		"BAIRAV_DLP_UPSTREAM": "https://api.anthropic.com/v1",
		"BAIRAV_DLP_PROVIDER": "anthropic",
		"BAIRAV_DLP_MODEL":    "claude-sonnet-4-5",
		"BAIRAV_DLP_ACTION":   "redact",
		"BAIRAV_DLP_PACK":     "global-safe,india",
		"BAIRAV_DLP_LISTEN":   ":8080",
	} {
		if env[k].Value != want {
			t.Errorf("%s = %q, want %q", k, env[k].Value, want)
		}
	}
	key := env["BAIRAV_PROVIDER_API_KEY"]
	if key.Value != "" || key.ValueFrom == nil || key.ValueFrom.SecretKeyRef.Name != "ant" {
		t.Errorf("API key must come from the Secret, never a literal: %+v", key)
	}
	if c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("readiness must be the local /healthz (no provider call), got %s", c.ReadinessProbe.HTTPGet.Path)
	}
	if d.Spec.Template.Labels["app.kubernetes.io/instance"] != "support" {
		t.Error("gateway pods must keep the model labels (Service selector, podProblem, status)")
	}
}

func TestBuildDeployment_OpenAICompatibleAndLocal(t *testing.T) {
	r := &ModelReconciler{}
	m := cloudModel("openai-compatible", &orchv1alpha1.ModelCloudSpec{BaseURL: "http://vllm.gpu:8000/v1", DLPAction: "block"})
	env := envMap(r.buildDeployment(m).Spec.Template.Spec.Containers[0])
	if env["BAIRAV_DLP_UPSTREAM"].Value != "http://vllm.gpu:8000/v1" || env["BAIRAV_DLP_ACTION"].Value != "block" {
		t.Errorf("openai-compatible env wrong: %+v", env)
	}
	if _, ok := env["BAIRAV_PROVIDER_API_KEY"]; ok {
		t.Error("no secret ref → no key env")
	}

	local := cloudModel("local", nil)
	local.Spec.Repo = "Qwen/Qwen2.5-0.5B-Instruct-GGUF"
	c := r.buildDeployment(local).Spec.Template.Spec.Containers[0]
	if c.Name != "serving" || c.Image != "bairav/serving:laptop" {
		t.Errorf("local models must be unchanged: %s %s", c.Name, c.Image)
	}
}

func TestSecurityContexts(t *testing.T) {
	r := &ModelReconciler{}
	check := func(name string, sc *corev1.SecurityContext, uid int64, nonRoot bool) {
		t.Helper()
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
			sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.SeccompProfile == nil {
			t.Fatalf("%s: not hardened: %+v", name, sc)
		}
		if nonRoot && (sc.RunAsUser == nil || *sc.RunAsUser != uid || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot) {
			t.Errorf("%s: want runAsNonRoot uid %d, got %+v", name, uid, sc)
		}
		if !nonRoot && sc.RunAsUser != nil {
			t.Errorf("%s: custom image must keep its own user", name)
		}
	}
	gw := cloudModel("openai", &orchv1alpha1.ModelCloudSpec{APIKeySecretRef: &orchv1alpha1.SecretKeyRef{Name: "k", Key: "k"}})
	check("gateway", r.buildDeployment(gw).Spec.Template.Spec.Containers[0].SecurityContext, dlpUID, true)

	local := cloudModel("local", nil)
	check("serving", r.buildDeployment(local).Spec.Template.Spec.Containers[0].SecurityContext, servingUID, true)

	local.Spec.Image = "ghcr.io/acme/vllm:1"
	check("custom serving", r.buildDeployment(local).Spec.Template.Spec.Containers[0].SecurityContext, 0, false)
}
