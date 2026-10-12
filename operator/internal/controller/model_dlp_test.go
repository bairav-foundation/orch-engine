// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func dlpModel() *orchv1alpha1.Model {
	m := &orchv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "shared-models"}}
	m.Spec.Repo = "Qwen/Qwen2.5-0.5B-Instruct-GGUF"
	m.Spec.DLPRef = &orchv1alpha1.LocalObjectReference{Name: "prod-guardrails"}
	return m
}

func TestWithModelDLP(t *testing.T) {
	r := &ModelReconciler{}
	m := dlpModel()
	pol := &resolvedDLPPolicy{name: "prod-guardrails", packs: []string{"global-safe", "india"},
		defaultAction: orchv1alpha1.DLPActionRedact, direction: orchv1alpha1.DLPDirectionBoth,
		auditPath: "-", customRulesCount: 2, hash: "abc123"}

	d := r.buildDeployment(m)
	withModelDLP(d, pol, modelPort(m))
	spec := d.Spec.Template.Spec

	if len(spec.Containers) != 2 {
		t.Fatalf("want serving + dlp containers, got %d", len(spec.Containers))
	}
	srv, proxy := spec.Containers[0], spec.Containers[1]

	// The serving container must be unreachable from the network.
	se := envMap(srv)
	if se["BAIRAV_HOST"].Value != "127.0.0.1" || se["BAIRAV_PORT"].Value != "18080" {
		t.Errorf("serving should listen on 127.0.0.1:18080, got host=%q port=%q", se["BAIRAV_HOST"].Value, se["BAIRAV_PORT"].Value)
	}
	if len(srv.Ports) != 0 {
		t.Errorf("serving container must not expose ports, got %v", srv.Ports)
	}
	for name, pr := range map[string]bool{"readiness": srv.ReadinessProbe != nil && srv.ReadinessProbe.Exec != nil,
		"liveness": srv.LivenessProbe != nil && srv.LivenessProbe.Exec != nil} {
		if !pr {
			t.Errorf("serving %s probe should exec curl on loopback", name)
		}
	}
	if srv.ReadinessProbe != nil && srv.ReadinessProbe.Exec != nil &&
		!strings.Contains(strings.Join(srv.ReadinessProbe.Exec.Command, " "), "http://127.0.0.1:18080/health") {
		t.Errorf("readiness probe command: %v", srv.ReadinessProbe.Exec.Command)
	}

	// The DLP proxy takes the client-facing port and forwards to loopback.
	pe := envMap(proxy)
	if proxy.Name != "dlp" || proxy.Image != DLPSidecarImage {
		t.Errorf("proxy container: name=%q image=%q", proxy.Name, proxy.Image)
	}
	if pe["BAIRAV_DLP_LISTEN"].Value != ":8080" || pe["BAIRAV_DLP_UPSTREAM"].Value != "http://127.0.0.1:18080" {
		t.Errorf("proxy listen/upstream: %q -> %q", pe["BAIRAV_DLP_LISTEN"].Value, pe["BAIRAV_DLP_UPSTREAM"].Value)
	}
	if pe["BAIRAV_DLP_PACK"].Value != "global-safe,india" {
		t.Errorf("packs: %q", pe["BAIRAV_DLP_PACK"].Value)
	}
	if len(proxy.Ports) != 1 || proxy.Ports[0].ContainerPort != 8080 {
		t.Errorf("proxy ports: %v", proxy.Ports)
	}
	if proxy.ReadinessProbe == nil || proxy.ReadinessProbe.HTTPGet == nil || proxy.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Error("proxy should be probed on its own /healthz (not audited, not forwarded)")
	}

	// Custom rules arrive through the policy ConfigMap; edits roll the Pod.
	foundVol := false
	for _, v := range spec.Volumes {
		if v.Name == dlpPolicyVolumeName && v.ConfigMap != nil && v.ConfigMap.Name == DLPPolicyConfigMapName("prod-guardrails") {
			foundVol = true
		}
	}
	if !foundVol {
		t.Error("custom-rules ConfigMap volume missing")
	}
	if d.Spec.Template.Annotations["orch.bairavs.com/dlp-policy-hash"] != "abc123" {
		t.Errorf("policy hash annotation: %v", d.Spec.Template.Annotations)
	}

	// The Service is unchanged: same port, same target, so callers don't change.
	svc := r.buildService(m)
	if svc.Spec.Ports[0].Port != 8080 || svc.Spec.Ports[0].TargetPort.IntValue() != 8080 {
		t.Errorf("service ports changed: %+v", svc.Spec.Ports[0])
	}
}

func TestWithModelDLPNoCustomRules(t *testing.T) {
	r := &ModelReconciler{}
	m := dlpModel()
	d := r.buildDeployment(m)
	withModelDLP(d, &resolvedDLPPolicy{name: "p", auditPath: "-"}, modelPort(m))
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == dlpPolicyVolumeName {
			t.Error("no custom rules: no policy volume expected")
		}
	}
	if envMap(d.Spec.Template.Spec.Containers[1])["BAIRAV_DLP_PACK"].Value != "none" {
		t.Error("a policy with no packs means custom rules only: pack must be none")
	}
}

func TestValidateModelRejectsDLPRefOnCloud(t *testing.T) {
	m := cloudModel(orchv1alpha1.ModelProviderOpenAI, &orchv1alpha1.ModelCloudSpec{
		APIKeySecretRef: &orchv1alpha1.SecretKeyRef{Name: "k", Key: "api-key"},
	})
	m.Spec.DLPRef = &orchv1alpha1.LocalObjectReference{Name: "p"}
	if got := validateModel(m); !strings.Contains(got, "dlpRef is for local models") {
		t.Errorf("validateModel = %q", got)
	}
}

func TestSnapshotDLPPolicyDefaults(t *testing.T) {
	p := &orchv1alpha1.DLPPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	s := snapshotDLPPolicy(p)
	if s.defaultAction != orchv1alpha1.DLPActionRedact || s.direction != orchv1alpha1.DLPDirectionBoth || s.auditPath != "-" {
		t.Errorf("defaults not applied: %+v", s)
	}
}
