// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

// vLLM backend (experimental): spec.backend: vllm.
//
// llama-server is the default because it runs anywhere, CPU included. On a
// GPU node vLLM serves far more requests per second, so a Model can ask for
// it instead. The Pod runs the upstream vllm/vllm-openai image with the
// Model's repo (a safetensors repo, not a GGUF) and keeps the same
// contract: an OpenAI-compatible API on the Service port, /health for
// probes, and the same Service and status endpoint. Agents, DLP (agent
// sidecars and spec.dlpRef) and upgrades work unchanged.

import (
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// VLLMImage is the default vLLM serving image. Pinned so a vLLM release
// can't change behaviour under a running cluster; override per Model with
// spec.image or for the operator with BAIRAV_VLLM_IMAGE.
var VLLMImage = envOr("BAIRAV_VLLM_IMAGE", "vllm/vllm-openai:v0.20.2")

func isVLLM(m *orchv1alpha1.Model) bool { return m.Spec.Backend == orchv1alpha1.ModelBackendVLLM }

// vllmArgs are the server flags. The model answers to its repo id, the
// Model's name and "default", so clients that send any of them work
// (llama-server ignores the field; vLLM would 404 on a name it doesn't know).
func vllmArgs(m *orchv1alpha1.Model, host string, port int32) []string {
	args := []string{
		"--model", m.Spec.Repo,
		"--host", host,
		"--port", strconv.Itoa(int(port)),
		"--served-model-name", m.Spec.Repo, m.Name, "default",
	}
	if m.Spec.Revision != "" && m.Spec.Revision != "main" {
		args = append(args, "--revision", m.Spec.Revision)
	}
	if m.Spec.ContextSize > 0 {
		args = append(args, "--max-model-len", strconv.Itoa(int(m.Spec.ContextSize)))
	}
	return args
}

// vllmResources: the user's resources over GPU defaults. A spec that only
// sets nvidia.com/gpu (what `--gpus N` writes) still gets CPU and memory.
func vllmResources(user corev1.ResourceRequirements) corev1.ResourceRequirements {
	r := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("32Gi"),
			"nvidia.com/gpu":      resource.MustParse("1"),
		},
	}
	for k, v := range user.Requests {
		r.Requests[k] = v
	}
	for k, v := range user.Limits {
		r.Limits[k] = v
	}
	return r
}

// buildVLLMDeployment is buildDeployment for spec.backend: vllm.
func (r *ModelReconciler) buildVLLMDeployment(m *orchv1alpha1.Model, labels map[string]string, port int32) *appsv1.Deployment {
	image := m.Spec.Image
	if image == "" {
		image = VLLMImage
	}
	env := []corev1.EnvVar{
		// Same marker the llama-server Pod carries; status and upgrades read it.
		{Name: "BAIRAV_REPO", Value: m.Spec.Repo},
		{Name: "HF_HOME", Value: "/models/hf"},
	}
	if m.Spec.HFTokenSecretRef != nil {
		env = append(env, corev1.EnvVar{Name: "HF_TOKEN", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: m.Spec.HFTokenSecretRef.Name},
				Key:                  m.Spec.HFTokenSecretRef.Key,
			},
		}})
	}
	health := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(port)}}
	shm := resource.MustParse("2Gi")
	one := int32(1)
	progressDeadline := int32(3600) // weights download + CUDA graph capture
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: m.Name, Namespace: m.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas:                &one,
			Selector:                &metav1.LabelSelector{MatchLabels: labels},
			Strategy:                deploymentStrategy(m.Spec.UpgradeStrategy),
			ProgressDeadlineSeconds: &progressDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:      "serving",
						Image:     image,
						Args:      vllmArgs(m, "0.0.0.0", port),
						Env:       env,
						Resources: vllmResources(m.Spec.Resources),
						// The upstream image runs as root; the same
						// hardening as any non-bairav serving image.
						SecurityContext: servingSecurityContext(image),
						Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        health,
							InitialDelaySeconds: 15,
							PeriodSeconds:       10,
							FailureThreshold:    180, // up to 30 min for a big download
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        health,
							InitialDelaySeconds: 1800,
							PeriodSeconds:       30,
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "models", MountPath: "/models"},
							{Name: "dshm", MountPath: "/dev/shm"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "models", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						// PyTorch shares tensors between processes via /dev/shm;
						// the container default (64Mi) is too small.
						{Name: "dshm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium: corev1.StorageMediumMemory, SizeLimit: &shm,
						}}},
					},
				},
			},
		},
	}
}

// rebindVLLM moves a vLLM serving container (recognised by its --host /
// --port args) to 127.0.0.1:port for model-side DLP and reports whether it
// did. The vLLM image has Python but not necessarily curl, so the loopback
// probes use Python.
func rebindVLLM(c *corev1.Container, port int32) bool {
	hostAt, portAt := -1, -1
	for i, a := range c.Args {
		switch a {
		case "--host":
			hostAt = i + 1
		case "--port":
			portAt = i + 1
		}
	}
	if hostAt <= 0 || portAt <= 0 || hostAt >= len(c.Args) || portAt >= len(c.Args) {
		return false
	}
	c.Args[hostAt], c.Args[portAt] = "127.0.0.1", strconv.Itoa(int(port))
	probe := corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
		"python3", "-c",
		fmt.Sprintf("import urllib.request; urllib.request.urlopen('http://127.0.0.1:%d/health', timeout=3)", port),
	}}}
	for _, pr := range []*corev1.Probe{c.ReadinessProbe, c.LivenessProbe} {
		if pr != nil {
			pr.ProbeHandler = probe
		}
	}
	return true
}

// validateVLLM checks the fields vLLM doesn't use.
func validateVLLM(m *orchv1alpha1.Model) string {
	if !isVLLM(m) {
		return ""
	}
	if m.Spec.IsCloud() {
		return "spec.backend applies to local models; a cloud provider runs its own engine"
	}
	if m.Spec.File != "" {
		return "spec.file picks a GGUF for llama-server; vLLM serves the whole repo (use a safetensors repo and drop spec.file)"
	}
	return ""
}
