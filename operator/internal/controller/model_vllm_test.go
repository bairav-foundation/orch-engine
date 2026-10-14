// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func vllmModel() *orchv1alpha1.Model {
	m := &orchv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "qwen-gpu", Namespace: "ml"}}
	m.Spec.Repo = "Qwen/Qwen2.5-1.5B-Instruct"
	m.Spec.Backend = orchv1alpha1.ModelBackendVLLM
	m.Spec.ContextSize = 8192
	return m
}

func TestBuildVLLMDeployment(t *testing.T) {
	r := &ModelReconciler{}
	m := vllmModel()
	m.Spec.Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")}
	d := r.buildDeployment(m)
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != VLLMImage {
		t.Errorf("image = %q", c.Image)
	}
	args := strings.Join(c.Args, " ")
	for _, want := range []string{"--model Qwen/Qwen2.5-1.5B-Instruct", "--host 0.0.0.0", "--port 8080",
		"--served-model-name Qwen/Qwen2.5-1.5B-Instruct qwen-gpu default", "--max-model-len 8192"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	gpu := c.Resources.Limits["nvidia.com/gpu"]
	if gpu.String() != "2" {
		t.Errorf("user GPU count should win: %s", gpu.String())
	}
	if _, ok := c.Resources.Requests[corev1.ResourceMemory]; !ok {
		t.Error("GPU-only resources should still get memory defaults")
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/health" {
		t.Error("readiness should be GET /health")
	}
	shm := false
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "dshm" && v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory {
			shm = true
		}
	}
	if !shm {
		t.Error("vLLM needs a memory-backed /dev/shm")
	}
}

func TestVLLMWithModelDLP(t *testing.T) {
	r := &ModelReconciler{}
	m := vllmModel()
	d := r.buildDeployment(m)
	withModelDLP(d, &resolvedDLPPolicy{name: "p", defaultAction: orchv1alpha1.DLPActionRedact,
		direction: orchv1alpha1.DLPDirectionBoth, auditPath: "-"}, modelPort(m))
	c := d.Spec.Template.Spec.Containers[0]
	args := strings.Join(c.Args, " ")
	if !strings.Contains(args, "--host 127.0.0.1") || !strings.Contains(args, "--port 18080") {
		t.Errorf("vLLM should be rebound to loopback: %q", args)
	}
	if c.ReadinessProbe.Exec == nil || c.ReadinessProbe.Exec.Command[0] != "python3" {
		t.Errorf("loopback probe should use python: %+v", c.ReadinessProbe)
	}
	if _, ok := envMap(c)["BAIRAV_HOST"]; ok {
		t.Error("BAIRAV_HOST is for the llama-server image only")
	}
	if len(d.Spec.Template.Spec.Containers) != 2 {
		t.Error("dlp container missing")
	}
}

func TestValidateVLLM(t *testing.T) {
	m := vllmModel()
	if p := validateModel(m); p != "" {
		t.Fatalf("valid vLLM model rejected: %s", p)
	}
	m.Spec.File = "model.gguf"
	if validateModel(m) == "" {
		t.Error("spec.file with vLLM should be rejected")
	}
}
