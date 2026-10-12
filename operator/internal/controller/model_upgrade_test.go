// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func TestDeploymentStrategy(t *testing.T) {
	s := deploymentStrategy("")
	if s.Type != appsv1.RollingUpdateDeploymentStrategyType || s.RollingUpdate == nil {
		t.Fatalf("default must be a rolling update, got %+v", s)
	}
	if s.RollingUpdate.MaxUnavailable.IntValue() != 0 || s.RollingUpdate.MaxSurge.IntValue() != 1 {
		t.Errorf("surge must be maxUnavailable=0 maxSurge=1 (old pod serves until new is Ready), got %+v", s.RollingUpdate)
	}
	r := deploymentStrategy(orchv1alpha1.ModelUpgradeRecreate)
	if r.Type != appsv1.RecreateDeploymentStrategyType || r.RollingUpdate != nil {
		t.Errorf("Recreate must not carry rollingUpdate params (API rejects it), got %+v", r)
	}
}

func deploy(gen, observed int64, replicas, updated, available int32) *appsv1.Deployment {
	one := int32(1)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: gen}}
	d.Spec.Replicas = &one
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: observed, Replicas: replicas,
		UpdatedReplicas: updated, AvailableReplicas: available, ReadyReplicas: available}
	return d
}

func modelAt(repo, servedRepo string) *orchv1alpha1.Model {
	m := &orchv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns", Generation: 2}}
	m.Spec.Repo = repo
	m.Status.ServedRepo = servedRepo
	return m
}

var svc = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"}}

// Mid-upgrade: old pod Ready (so phase stays Ready), new pod not yet.
// ServedRepo must still name the OLD model, and Upgrading must be True.
func TestComputeStatus_MidUpgrade(t *testing.T) {
	r := &ModelReconciler{}
	st := r.computeStatus(modelAt("org/new", "org/old"), deploy(3, 3, 2, 1, 1), svc)
	if st.Phase != orchv1alpha1.ModelPhaseReady {
		t.Errorf("old pod still serves, phase should stay Ready, got %s", st.Phase)
	}
	if st.ServedRepo != "org/old" {
		t.Errorf("ServedRepo moved before rollout finished: %q", st.ServedRepo)
	}
	c := meta.FindStatusCondition(st.Conditions, "Upgrading")
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "RollingOut" {
		t.Errorf("want Upgrading=True/RollingOut, got %+v", c)
	}
}

// Done: every replica on the new template and available → Served* flip,
// Upgrading cleared.
func TestComputeStatus_UpgradeComplete(t *testing.T) {
	r := &ModelReconciler{}
	m := modelAt("org/new", "org/old")
	m.Status.Conditions = []metav1.Condition{{Type: "Upgrading", Status: metav1.ConditionTrue, Reason: "RollingOut"}}
	st := r.computeStatus(m, deploy(3, 3, 1, 1, 1), svc)
	if st.ServedRepo != "org/new" {
		t.Errorf("ServedRepo = %q, want org/new", st.ServedRepo)
	}
	if meta.FindStatusCondition(st.Conditions, "Upgrading") != nil {
		t.Error("Upgrading condition should be cleared once the rollout completes")
	}
}

// Stalled: K8s says the progress deadline passed → UpgradeStalled, and the
// old model is still the one serving.
func TestComputeStatus_UpgradeStalled(t *testing.T) {
	r := &ModelReconciler{}
	d := deploy(3, 3, 2, 1, 1)
	d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
	st := r.computeStatus(modelAt("org/new", "org/old"), d, svc)
	c := meta.FindStatusCondition(st.Conditions, "Upgrading")
	if c == nil || c.Reason != "UpgradeStalled" {
		t.Errorf("want Upgrading/UpgradeStalled, got %+v", c)
	}
	if st.ServedRepo != "org/old" {
		t.Errorf("a stalled upgrade must not claim the new model is serving, got %q", st.ServedRepo)
	}
}

// First deploy: ServedRepo empty → set once the first rollout completes.
func TestComputeStatus_FirstDeploySetsServed(t *testing.T) {
	r := &ModelReconciler{}
	st := r.computeStatus(modelAt("org/first", ""), deploy(1, 1, 1, 1, 1), svc)
	if st.ServedRepo != "org/first" {
		t.Errorf("ServedRepo = %q, want org/first", st.ServedRepo)
	}
}
