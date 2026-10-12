// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func TestHumanAge(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cases := map[time.Duration]string{
		45 * time.Second:         "45s",
		12 * time.Minute:         "12m",
		5 * time.Hour:            "5h",
		3 * 24 * time.Hour:       "3d",
		3 * 365 * 24 * time.Hour: "3y",
	}
	for d, want := range cases {
		if got := humanAge(now, now.Add(-d)); got != want {
			t.Errorf("humanAge(%s) = %q, want %q", d, got, want)
		}
	}
	if humanAge(now, time.Time{}) != "-" {
		t.Error("zero time should print -")
	}
}

func testModel(repo, served string, conds ...metav1.Condition) orchv1alpha1.Model {
	m := orchv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team-a",
		CreationTimestamp: metav1.NewTime(time.Now().Add(-3 * time.Hour))}}
	m.Spec.Repo = repo
	m.Status.ServedRepo = served
	m.Status.Phase = orchv1alpha1.ModelPhaseReady
	m.Status.Conditions = conds
	return m
}

func TestK8sStatusRow(t *testing.T) {
	r := k8sStatusRow(testModel("org/new", "org/old",
		metav1.Condition{Type: "Upgrading", Status: metav1.ConditionTrue, Reason: "RollingOut"}))
	if r.serving != "org/old" || r.wanted != "org/new" || !strings.HasPrefix(r.upgrade, "rolling out → new") {
		t.Errorf("mid-upgrade row wrong: %+v", r)
	}
	if r.strategy != "Surge" || humanAge(time.Now(), r.created) != "3h" {
		t.Errorf("strategy/age wrong: %q %q", r.strategy, humanAge(time.Now(), r.created))
	}

	r = k8sStatusRow(testModel("org/new", "org/old",
		metav1.Condition{Type: "Upgrading", Status: metav1.ConditionFalse, Reason: "UpgradeStalled"}))
	if !strings.HasPrefix(r.upgrade, "STALLED") {
		t.Errorf("stalled upgrade not flagged: %q", r.upgrade)
	}

	// Ready but no servedRepo = operator from before Day 27; say so
	// instead of pretending.
	r = k8sStatusRow(testModel("org/x", ""))
	if !strings.Contains(r.upgrade, "operator too old") {
		t.Errorf("old operator not flagged: %q", r.upgrade)
	}

	m := testModel("org/new", "org/new")
	m.Annotations = map[string]string{annPrevRepo: "org/old",
		annUpgradedAt: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)}
	r = k8sStatusRow(m)
	if r.prev != "org/old" || r.upgrade != "from old 10m ago" || !strings.Contains(r.rollbackHint, "--namespace team-a") {
		t.Errorf("previous-model info wrong: %+v", r)
	}
}

func TestDockerStatusPhase(t *testing.T) {
	for in, want := range map[string]string{
		"Exited (0) 2 months ago":         "Stopped",
		"Exited (137) 61 days ago":        "Exited (137)",
		"Up 3 minutes (healthy)":          "Ready",
		"Up 5 seconds (health: starting)": "Deploying",
	} {
		if got := dockerStatusPhase(in); got != want {
			t.Errorf("dockerStatusPhase(%q) = %q, want %q", in, got, want)
		}
	}
}
