// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func TestEgressPolicyOnlyWhenRestricted(t *testing.T) {
	labels := map[string]string{"orch.bairavs.com/agent": "bot"}
	for name, spec := range map[string]*orchv1alpha1.EgressSpec{
		"nil":   nil,
		"empty": {},
		"open":  {Mode: orchv1alpha1.EgressOpen},
	} {
		if np, _ := buildEgressPolicy("bot", "team-a", labels, "orch.bairavs.com/agent", spec, nil); np != nil {
			t.Errorf("%s: want no policy, got one", name)
		}
	}
}

func TestEgressPolicyRestricted(t *testing.T) {
	a := &orchv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "team-a"}}
	a.Spec.ModelRefs = []orchv1alpha1.LocalObjectReference{{Name: "qwen", Namespace: "shared-models"}}
	a.Spec.SkillRefs = []orchv1alpha1.LocalObjectReference{{Name: "tickets"}}
	a.Spec.Egress = &orchv1alpha1.EgressSpec{Mode: orchv1alpha1.EgressRestricted, Allow: []orchv1alpha1.EgressRule{
		{CIDR: "203.0.113.7/32", Ports: []int32{443}},
		{CIDR: "10.20.0.0/16"},
		{CIDR: "not-a-cidr"},
	}}
	labels := map[string]string{"orch.bairavs.com/agent": "bot", "app.kubernetes.io/name": "bairav-agent"}

	np, bad := buildEgressPolicy(a.Name, a.Namespace, labels, "orch.bairavs.com/agent", a.Spec.Egress, agentEgressPeers(a))
	if np == nil {
		t.Fatal("want a policy")
	}
	if len(bad) != 1 || bad[0] != "not-a-cidr" {
		t.Errorf("bad CIDRs = %v, want [not-a-cidr]", bad)
	}
	if np.Name != "bot-egress" || np.Namespace != "team-a" {
		t.Errorf("name = %s/%s", np.Namespace, np.Name)
	}
	if got := np.Spec.PodSelector.MatchLabels; len(got) != 1 || got["orch.bairavs.com/agent"] != "bot" {
		t.Errorf("pod selector = %v", got)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Errorf("policy types = %v, want only Egress (ingress untouched)", np.Spec.PolicyTypes)
	}

	// DNS + 2 peers + 2 valid CIDRs.
	rules := np.Spec.Egress
	if len(rules) != 5 {
		t.Fatalf("want 5 egress rules, got %d: %+v", len(rules), rules)
	}
	if len(rules[0].To) != 0 || len(rules[0].Ports) != 2 || rules[0].Ports[0].Port.IntVal != 53 ||
		*rules[0].Ports[0].Protocol != corev1.ProtocolUDP || *rules[0].Ports[1].Protocol != corev1.ProtocolTCP {
		t.Errorf("rule 0 should be DNS on UDP+TCP 53: %+v", rules[0])
	}
	model := rules[1].To[0]
	if model.NamespaceSelector.MatchLabels[nsNameLabel] != "shared-models" || model.PodSelector.MatchLabels["orch.bairavs.com/model"] != "qwen" {
		t.Errorf("model peer = %+v / %+v", model.NamespaceSelector, model.PodSelector)
	}
	skill := rules[2].To[0]
	if skill.NamespaceSelector.MatchLabels[nsNameLabel] != "team-a" || skill.PodSelector.MatchLabels["orch.bairavs.com/skill"] != "tickets" {
		t.Errorf("skill peer should default to the agent's namespace: %+v / %+v", skill.NamespaceSelector, skill.PodSelector)
	}
	if rules[3].To[0].IPBlock.CIDR != "203.0.113.7/32" || len(rules[3].Ports) != 1 || rules[3].Ports[0].Port.IntVal != 443 {
		t.Errorf("rule 3 = %+v", rules[3])
	}
	if rules[4].To[0].IPBlock.CIDR != "10.20.0.0/16" || len(rules[4].Ports) != 0 {
		t.Errorf("rule 4 should allow all ports: %+v", rules[4])
	}
}

func TestWorkloadPodsGetNoServiceAccountToken(t *testing.T) {
	s := &orchv1alpha1.Skill{ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "team-a"}}
	s.Spec.Image = "bairav/mcp-echo:laptop"
	d := (&SkillReconciler{}).buildDeployment(s)
	if v := d.Spec.Template.Spec.AutomountServiceAccountToken; v == nil || *v {
		t.Errorf("skill pod: automountServiceAccountToken = %v, want false", v)
	}
	s.Spec.Image, s.Spec.URL = "", "https://mcp.example.com/mcp"
	gw := buildRemoteSkillDeployment(s, d.Labels, observeOnlyPolicy())
	if v := gw.Spec.Template.Spec.AutomountServiceAccountToken; v == nil || *v {
		t.Errorf("remote skill gateway pod: automountServiceAccountToken = %v, want false", v)
	}
}
