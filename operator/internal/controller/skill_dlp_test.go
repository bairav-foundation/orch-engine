// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func testPolicy() *resolvedDLPPolicy {
	return &resolvedDLPPolicy{name: "tools-guard", packs: []string{"global-safe", "india"},
		defaultAction: orchv1alpha1.DLPActionRedact, direction: orchv1alpha1.DLPDirectionBoth,
		auditPath: "-", hash: "h1"}
}

func TestWithSkillDLP(t *testing.T) {
	r := &SkillReconciler{}
	s := &orchv1alpha1.Skill{ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "team-a"}}
	s.Spec.Image = "bairav/mcp-echo:laptop"
	s.Spec.DLPRef = &orchv1alpha1.LocalObjectReference{Name: "tools-guard"}

	d := r.buildDeployment(s)
	withSkillDLP(d, testPolicy(), s)
	cs := d.Spec.Template.Spec.Containers
	if len(cs) != 2 {
		t.Fatalf("want skill + dlp containers, got %d", len(cs))
	}
	if cs[0].ReadinessProbe != nil || cs[0].LivenessProbe != nil {
		t.Error("skill probes should move to the DLP container (the NetworkPolicy closes the skill port)")
	}
	pe := envMap(cs[1])
	for k, want := range map[string]string{
		"BAIRAV_DLP_MODE":            "mcp",
		"BAIRAV_DLP_LISTEN":          ":18090",
		"BAIRAV_DLP_UPSTREAM":        "http://127.0.0.1:3000",
		"BAIRAV_DLP_UPSTREAM_HEALTH": "/health",
		"BAIRAV_DLP_PACK":            "global-safe,india",
	} {
		if pe[k].Value != want {
			t.Errorf("%s = %q, want %q", k, pe[k].Value, want)
		}
	}
	if d.Spec.Template.Annotations["orch.bairavs.com/dlp-policy-hash"] != "h1" {
		t.Error("policy hash should be stamped on the Pod template")
	}

	svc := r.buildService(s)
	if svc.Spec.Ports[0].TargetPort.IntVal != skillDLPPort || svc.Spec.Ports[0].Port != 3000 {
		t.Errorf("Service should keep port 3000 and target the DLP port: %+v", svc.Spec.Ports[0])
	}

	np := buildSkillNetworkPolicy(s, d.Spec.Template.Labels)
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].Ports) != 1 || np.Spec.Ingress[0].Ports[0].Port.IntVal != skillDLPPort {
		t.Errorf("NetworkPolicy should admit only the DLP port: %+v", np.Spec.Ingress)
	}
	if np.Spec.PodSelector.MatchLabels["orch.bairavs.com/skill"] != "echo" {
		t.Errorf("NetworkPolicy selects the wrong pods: %v", np.Spec.PodSelector.MatchLabels)
	}
}

func TestRemoteSkill(t *testing.T) {
	r := &SkillReconciler{}
	s := &orchv1alpha1.Skill{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "team-a"}}
	s.Spec.URL = "https://mcp.example.com/sse"
	s.Spec.AuthSecretRef = &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "github-mcp"}, Key: "token"}
	if p := validateSkill(s); p != "" {
		t.Fatal(p)
	}

	labels := r.buildDeployment(s).Labels
	d := buildRemoteSkillDeployment(s, labels, nil)
	cs := d.Spec.Template.Spec.Containers
	if len(cs) != 1 || cs[0].Name != "gateway" || cs[0].Image != DLPSidecarImage {
		t.Fatalf("want one gateway container, got %+v", cs)
	}
	e := envMap(cs[0])
	if e["BAIRAV_DLP_UPSTREAM"].Value != "https://mcp.example.com" {
		t.Errorf("upstream should be the origin: %q", e["BAIRAV_DLP_UPSTREAM"].Value)
	}
	if e["BAIRAV_DLP_ACTION"].Value != string(orchv1alpha1.DLPActionAudit) {
		t.Errorf("without a dlpRef the gateway is audit-only: %q", e["BAIRAV_DLP_ACTION"].Value)
	}
	tok := e["BAIRAV_MCP_UPSTREAM_TOKEN"]
	if tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef == nil || tok.ValueFrom.SecretKeyRef.Name != "github-mcp" {
		t.Errorf("token should come from the Secret: %+v", tok)
	}
	if got := remotePath(s.Spec.URL); got != "/sse" {
		t.Errorf("remotePath = %q", got)
	}

	for name, bad := range map[string]func(*orchv1alpha1.Skill){
		"neither":       func(x *orchv1alpha1.Skill) { x.Spec.URL = "" },
		"both":          func(x *orchv1alpha1.Skill) { x.Spec.Image = "img" },
		"not http":      func(x *orchv1alpha1.Skill) { x.Spec.URL = "ftp://x" },
		"secret no url": func(x *orchv1alpha1.Skill) { x.Spec.URL = ""; x.Spec.Image = "img" },
	} {
		c := s.DeepCopy()
		bad(c)
		if validateSkill(c) == "" {
			t.Errorf("%s: expected a validation problem", name)
		}
	}
}

func TestToolPinReachesTheProxy(t *testing.T) {
	s := &orchv1alpha1.Skill{ObjectMeta: metav1.ObjectMeta{Name: "crm", Namespace: "team-a"}}
	s.Spec.URL = "https://mcp.crm.example.com/mcp"

	gw := buildRemoteSkillDeployment(s, map[string]string{"orch.bairavs.com/skill": "crm"}, nil).Spec.Template.Spec.Containers[0]
	env := envMap(gw)
	if env["BAIRAV_SKILL_NAME"].Value != "crm" {
		t.Errorf("BAIRAV_SKILL_NAME = %q", env["BAIRAV_SKILL_NAME"].Value)
	}
	if _, set := env["BAIRAV_MCP_TOOLS_PIN"]; set {
		t.Error("no pin in spec: BAIRAV_MCP_TOOLS_PIN should be unset (trust on first use)")
	}

	fp := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	s.Spec.ToolsPin = map[string]string{"search": fp, "create_ticket": fp}
	gw = buildRemoteSkillDeployment(s, map[string]string{"orch.bairavs.com/skill": "crm"}, nil).Spec.Template.Spec.Containers[0]
	want := `{"create_ticket":"` + fp + `","search":"` + fp + `"}`
	if got := envMap(gw)["BAIRAV_MCP_TOOLS_PIN"].Value; got != want {
		t.Errorf("BAIRAV_MCP_TOOLS_PIN = %s, want %s (sorted, so the Pod only rolls on a real change)", got, want)
	}

	img := &orchv1alpha1.Skill{ObjectMeta: metav1.ObjectMeta{Name: "kb", Namespace: "team-a"}}
	img.Spec.Image = "bairav/mcp-echo:laptop"
	img.Spec.ToolsPin = map[string]string{"echo": fp}
	d := (&SkillReconciler{}).buildDeployment(img)
	withSkillDLP(d, testPolicy(), img)
	if got := envMap(d.Spec.Template.Spec.Containers[1])["BAIRAV_MCP_TOOLS_PIN"].Value; got != `{"echo":"`+fp+`"}` {
		t.Errorf("image skill proxy pin = %s", got)
	}
}
