// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func TestDockerPhase(t *testing.T) {
	cases := map[string]string{
		"Up 3 minutes (healthy)":          "Ready",
		"Up 5 seconds (health: starting)": "Deploying",
		"Up 2 hours (unhealthy)":          "Failed",
		"Up 10 minutes":                   "Ready",
		"Restarting (1) 3 seconds ago":    "Deploying",
		"Exited (137) About a minute ago": "Failed",
	}
	for in, want := range cases {
		if got := dockerPhase(in); got != want {
			t.Errorf("dockerPhase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgentMessage(t *testing.T) {
	a := orchv1alpha1.Agent{}
	a.Status.Phase = orchv1alpha1.AgentPhaseWaitingForDeps
	a.Status.Conditions = []metav1.Condition{
		{Type: "Progressing", Status: metav1.ConditionTrue, Message: "no AccessGrant ..."},
	}
	if got := agentMessage(a); got != "no AccessGrant ..." {
		t.Errorf("waiting agent: got %q", got)
	}
	a.Status.Conditions = append(a.Status.Conditions,
		metav1.Condition{Type: "Degraded", Status: metav1.ConditionTrue, Message: "missing Models: qwen"})
	if got := agentMessage(a); got != "missing Models: qwen" {
		t.Errorf("Degraded should win, got %q", got)
	}
	a.Status.Phase = orchv1alpha1.AgentPhaseReady
	a.Status.Conditions = []metav1.Condition{{Type: "Progressing", Status: metav1.ConditionTrue, Message: "stale"}}
	if got := agentMessage(a); got != "" {
		t.Errorf("Ready agent should have no message, got %q", got)
	}
}

func TestRefsToStrings(t *testing.T) {
	got := refsToStrings("team-b", []orchv1alpha1.LocalObjectReference{{Name: "local"}, {Namespace: "team-a", Name: "qwen"}, {Namespace: "team-b", Name: "same"}})
	want := []string{"local", "team-a/qwen", "same"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}
