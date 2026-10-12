// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

// cloud Models. For provider openai / anthropic / openai-compatible
// the Model's Deployment runs the bairav DLP image in gateway mode instead
// of a model server. Same name, same Service, same status fields, so
// Agents, `model upgrade` (local ⇄ cloud included), `model status` and the
// dashboard all work unchanged.

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// providerBaseURL mirrors cli/internal/dlp.ProviderBaseURL (the operator
// module can't import the CLI's internal packages).
func providerBaseURL(provider string) string {
	switch provider {
	case orchv1alpha1.ModelProviderOpenAI:
		return "https://api.openai.com/v1"
	case orchv1alpha1.ModelProviderAnthropic:
		return "https://api.anthropic.com/v1"
	}
	return ""
}

// validateModel returns a user-facing reason the spec can't be served,
// or "" when it's fine. Checked before anything is created.
func validateModel(m *orchv1alpha1.Model) string {
	if !m.Spec.IsCloud() {
		return ""
	}
	c := m.Spec.Cloud
	if m.Spec.Provider == orchv1alpha1.ModelProviderOpenAICompatible && (c == nil || c.BaseURL == "") {
		return "provider openai-compatible needs spec.cloud.baseURL (the API base URL, e.g. https://host/v1)"
	}
	if m.Spec.Provider != orchv1alpha1.ModelProviderOpenAICompatible && (c == nil || c.APIKeySecretRef == nil) {
		return fmt.Sprintf("provider %s needs spec.cloud.apiKeySecretRef (a Secret holding the API key); "+
			"create one with: kubectl create secret generic %s-provider-key -n %s --from-literal=api-key=...",
			m.Spec.Provider, m.Name, m.Namespace)
	}
	return ""
}

// buildGatewayDeployment is buildDeployment for cloud Models.
func (r *ModelReconciler) buildGatewayDeployment(m *orchv1alpha1.Model, labels map[string]string, port int32) *appsv1.Deployment {
	c := m.Spec.Cloud
	if c == nil {
		c = &orchv1alpha1.ModelCloudSpec{}
	}
	image := c.Image
	if image == "" {
		image = DLPSidecarImage
	}
	upstream := c.BaseURL
	if upstream == "" {
		upstream = providerBaseURL(m.Spec.Provider)
	}
	action := c.DLPAction
	if action == "" {
		action = "redact"
	}
	packs := "global-safe"
	if len(c.DLPPacks) > 0 {
		packs = strings.Join(c.DLPPacks, ",")
	}

	env := []corev1.EnvVar{
		{Name: "BAIRAV_DLP_LISTEN", Value: fmt.Sprintf(":%d", port)},
		{Name: "BAIRAV_DLP_UPSTREAM", Value: upstream},
		{Name: "BAIRAV_DLP_PROVIDER", Value: m.Spec.Provider},
		{Name: "BAIRAV_DLP_MODEL", Value: m.Spec.Repo},
		{Name: "BAIRAV_DLP_ACTION", Value: action},
		{Name: "BAIRAV_DLP_PACK", Value: packs},
		{Name: "BAIRAV_DLP_DIRECTION", Value: "both"},
		{Name: "BAIRAV_DLP_AUDIT", Value: "-"},
	}
	if c.APIKeySecretRef != nil {
		env = append(env, corev1.EnvVar{
			Name: "BAIRAV_PROVIDER_API_KEY",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: c.APIKeySecretRef.Name},
				Key:                  c.APIKeySecretRef.Key,
			}},
		})
	}

	probe := func(delay, period int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt32(port),
			}},
			InitialDelaySeconds: delay,
			PeriodSeconds:       period,
		}
	}
	one := int32(1)
	progressDeadline := int32(300) // nothing to download: 5 min is plenty
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
						Name:  "gateway",
						Image: image,
						Env:   env,
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
						},
						ReadinessProbe:  probe(1, 5),
						LivenessProbe:   probe(10, 30),
						SecurityContext: hardenedSecurityContext(dlpUID, true),
					}},
				},
			},
		},
	}
}
