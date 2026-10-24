// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func TestModelPinReachesServing(t *testing.T) {
	m := &orchv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "shared-models"}}
	m.Spec.Repo = "Qwen/Qwen2.5-0.5B-Instruct-GGUF"
	m.Spec.File = "qwen2.5-0.5b-instruct-q4_k_m.gguf"
	m.Spec.Revision = "main"

	env := envMap((&ModelReconciler{}).buildDeployment(m).Spec.Template.Spec.Containers[0])
	if _, set := env["BAIRAV_REVISION"]; set {
		t.Error("revision main: BAIRAV_REVISION should be left to the entrypoint default")
	}
	if _, set := env["BAIRAV_SHA256"]; set {
		t.Error("no sha256 in spec: BAIRAV_SHA256 should be unset")
	}

	commit := strings.Repeat("9", 40)
	sum := strings.Repeat("7", 64)
	m.Spec.Revision, m.Spec.SHA256 = commit, sum
	d := (&ModelReconciler{}).buildDeployment(m)
	env = envMap(d.Spec.Template.Spec.Containers[0])
	if env["BAIRAV_REVISION"].Value != commit || env["BAIRAV_SHA256"].Value != sum {
		t.Errorf("pinned model env: revision=%q sha256=%q", env["BAIRAV_REVISION"].Value, env["BAIRAV_SHA256"].Value)
	}
	if v := d.Spec.Template.Spec.AutomountServiceAccountToken; v == nil || *v {
		t.Error("model pod should not get a service account token")
	}
}
