// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// TestComputeModelEndpointsAndSidecars_NoDLP verifies the no-DLP
// behaviour is preserved when no DLPPolicy is bound: model env-var
// endpoints remain the raw Service DNS, and no sidecars are created.
func TestComputeModelEndpointsAndSidecars_NoDLP(t *testing.T) {
	a := &orchv1alpha1.Agent{}
	deps := &resolvedWithFlag{
		resolved: resolved{
			models: []resolvedModel{
				{name: "qwen-tiny", endpoint: "http://qwen-tiny.default.svc.cluster.local:8080/v1"},
			},
			dlp: nil,
		},
		allReady: true,
	}
	endpoints, sidecars := computeModelEndpointsAndSidecars(a, deps)
	if len(sidecars) != 0 {
		t.Errorf("expected 0 sidecars when no DLPPolicy bound, got %d", len(sidecars))
	}
	got := endpoints["qwen-tiny"]
	if got != "http://qwen-tiny.default.svc.cluster.local:8080/v1" {
		t.Errorf("endpoint = %q, want raw Service DNS", got)
	}
}

// TestComputeModelEndpointsAndSidecars_WithDLP verifies that when a
// DLPPolicy is bound: one sidecar is emitted per Model, endpoints point
// at localhost:9100+, and every sidecar carries the policy config as
// BAIRAV_DLP_* env vars.
func TestComputeModelEndpointsAndSidecars_WithDLP(t *testing.T) {
	a := &orchv1alpha1.Agent{}
	deps := &resolvedWithFlag{
		resolved: resolved{
			models: []resolvedModel{
				{name: "qwen-tiny", endpoint: "http://qwen-tiny.default.svc.cluster.local:8080/v1"},
				{name: "gemma-2b", endpoint: "http://gemma-2b.default.svc.cluster.local:8080/v1"},
			},
			dlp: &resolvedDLPPolicy{
				name:          "prod-guardrails",
				packs:         []string{"global-safe", "india"},
				defaultAction: orchv1alpha1.DLPActionRedact,
				direction:     orchv1alpha1.DLPDirectionBoth,
				auditPath:     "-",
			},
		},
		allReady: true,
	}
	endpoints, sidecars := computeModelEndpointsAndSidecars(a, deps)

	if len(sidecars) != 2 {
		t.Fatalf("expected 2 sidecars (one per Model), got %d", len(sidecars))
	}
	if endpoints["qwen-tiny"] != "http://127.0.0.1:9100/v1" {
		t.Errorf("qwen-tiny endpoint = %q, want http://127.0.0.1:9100/v1", endpoints["qwen-tiny"])
	}
	if endpoints["gemma-2b"] != "http://127.0.0.1:9101/v1" {
		t.Errorf("gemma-2b endpoint = %q, want http://127.0.0.1:9101/v1", endpoints["gemma-2b"])
	}
	if sidecars[0].Name != "dlp-qwen-tiny" {
		t.Errorf("sidecar[0].Name = %q, want dlp-qwen-tiny", sidecars[0].Name)
	}
	if sidecars[0].Image != DLPSidecarImage {
		t.Errorf("sidecar[0].Image = %q, want %q", sidecars[0].Image, DLPSidecarImage)
	}

	// Every sidecar must carry the essential env vars.
	envMap := envAsMap(sidecars[0].Env)
	if got := envMap["BAIRAV_DLP_UPSTREAM"]; got != "http://qwen-tiny.default.svc.cluster.local:8080/v1" {
		t.Errorf("BAIRAV_DLP_UPSTREAM = %q, want raw Model Service DNS", got)
	}
	if got := envMap["BAIRAV_DLP_LISTEN"]; got != ":9100" {
		t.Errorf("BAIRAV_DLP_LISTEN = %q, want :9100", got)
	}
	if got := envMap["BAIRAV_DLP_ACTION"]; got != "redact" {
		t.Errorf("BAIRAV_DLP_ACTION = %q, want redact", got)
	}
	if got := envMap["BAIRAV_DLP_DIRECTION"]; got != "both" {
		t.Errorf("BAIRAV_DLP_DIRECTION = %q, want both", got)
	}
	if got := envMap["BAIRAV_DLP_PACK"]; got != "global-safe,india" {
		t.Errorf("BAIRAV_DLP_PACK = %q, want global-safe,india", got)
	}
}

// TestComputeModelEndpointsAndSidecars_NoPacksCustomRules — a policy with
// no packs but customRules sets BAIRAV_DLP_PACK=none (so the CLI's
// global-safe default doesn't sneak in) and mounts the projected
// ConfigMap with BAIRAV_DLP_POLICY pointing at it.
func TestComputeModelEndpointsAndSidecars_NoPacksCustomRules(t *testing.T) {
	a := &orchv1alpha1.Agent{}
	deps := &resolvedWithFlag{
		resolved: resolved{
			models: []resolvedModel{
				{name: "m", endpoint: "http://m.default.svc:8080/v1"},
			},
			dlp: &resolvedDLPPolicy{
				name:             "custom-only",
				packs:            nil,
				defaultAction:    orchv1alpha1.DLPActionRedact,
				direction:        orchv1alpha1.DLPDirectionBoth,
				auditPath:        "-",
				customRulesCount: 2,
				hash:             "abc123",
			},
		},
		allReady: true,
	}
	_, sidecars := computeModelEndpointsAndSidecars(a, deps)
	if len(sidecars) != 1 {
		t.Fatalf("expected 1 sidecar, got %d", len(sidecars))
	}
	envMap := envAsMap(sidecars[0].Env)
	if got := envMap["BAIRAV_DLP_PACK"]; got != "none" {
		t.Errorf("BAIRAV_DLP_PACK = %q, want none", got)
	}
	if got := envMap["BAIRAV_DLP_POLICY"]; got != "/etc/bairav/policy/policy.yaml" {
		t.Errorf("BAIRAV_DLP_POLICY = %q, want /etc/bairav/policy/policy.yaml", got)
	}
	if len(sidecars[0].VolumeMounts) != 1 || sidecars[0].VolumeMounts[0].MountPath != DLPPolicyMountDir {
		t.Errorf("sidecar should mount the policy ConfigMap at %s; got %+v", DLPPolicyMountDir, sidecars[0].VolumeMounts)
	}
}

// TestBuildDeployment_DLPVolumeAndHash — the Pod template carries the
// projected ConfigMap volume and the policy hash annotation that makes
// policy edits roll the Pod.
func TestBuildDeployment_DLPVolumeAndHash(t *testing.T) {
	a := &orchv1alpha1.Agent{}
	a.Name, a.Namespace = "support-agent", "default"
	a.Spec.Image = "nginx:alpine"
	deps := &resolvedWithFlag{
		resolved: resolved{
			models: []resolvedModel{{name: "m", endpoint: "http://m.default.svc:8080/v1"}},
			dlp: &resolvedDLPPolicy{
				name: "prod-guardrails", packs: []string{"global-safe"},
				defaultAction: orchv1alpha1.DLPActionRedact, direction: orchv1alpha1.DLPDirectionBoth,
				auditPath: "-", customRulesCount: 1, hash: "deadbeef",
			},
		},
		allReady: true,
	}
	d := (&AgentReconciler{}).buildDeployment(a, deps)
	tpl := d.Spec.Template
	if tpl.Annotations["orch.bairavs.com/dlp-policy-hash"] != "deadbeef" {
		t.Errorf("missing dlp-policy-hash annotation: %v", tpl.Annotations)
	}
	if len(tpl.Spec.Volumes) != 1 || tpl.Spec.Volumes[0].ConfigMap == nil ||
		tpl.Spec.Volumes[0].ConfigMap.Name != "dlppolicy-prod-guardrails" {
		t.Errorf("expected ConfigMap volume dlppolicy-prod-guardrails, got %+v", tpl.Spec.Volumes)
	}
	if len(tpl.Spec.Containers) != 2 {
		t.Errorf("expected agent + 1 sidecar, got %d containers", len(tpl.Spec.Containers))
	}
}

// TestRenderPolicyFile_CLIFormat — projected file is the CLI's policy v1
// shape, with CR action "audit" mapped to the CLI's "audit_only".
func TestRenderPolicyFile_CLIFormat(t *testing.T) {
	body, err := RenderPolicyFile([]orchv1alpha1.DLPCustomRule{
		{Label: "TICKET", Regex: `TKT-\d{6}`, Action: orchv1alpha1.DLPActionAudit},
		{Label: "IBAN", Regex: `[A-Z]{2}\d{2}`, Checksum: "MOD11"},
	})
	if err != nil {
		t.Fatalf("RenderPolicyFile: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"version": "v1"`, `"pattern": "TKT-\\d{6}"`, `"action": "audit_only"`, `"checksum": "mod11"`} {
		if !strings.Contains(s, want) {
			t.Errorf("policy file missing %s:\n%s", want, s)
		}
	}
}

// TestDLPContainerName_LongNameTruncated verifies dlpContainerName
// keeps the container name inside K8s's 63-char limit even for
// pathologically long Model names.
func TestDLPContainerName_LongNameTruncated(t *testing.T) {
	long := strings.Repeat("a", 80)
	got := dlpContainerName(long)
	if len(got) > 63 {
		t.Errorf("dlpContainerName(%d chars) returned %d chars, want <=63", len(long), len(got))
	}
	if !strings.HasPrefix(got, "dlp-") {
		t.Errorf("dlpContainerName should always prefix with dlp-, got %q", got)
	}
}

// envAsMap flattens a []corev1.EnvVar (name/value only — we don't test
// valueFrom shapes here) into name→value for readable assertions.
func envAsMap(env []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}
