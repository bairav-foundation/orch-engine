// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

// DLP on Skill traffic, and remote (URL) Skills.
//
// Image Skill with spec.dlpRef:
//
//	Service :port ──▶ dlp container :18090 (MCP mode) ──▶ 127.0.0.1:<port> MCP server
//
// MCP server images pick their own bind address, so unlike a Model we
// can't force loopback. A NetworkPolicy owned by the Skill admits traffic
// to the Pod on the DLP port only, which closes the server's own port to
// the rest of the cluster (on a CNI that enforces NetworkPolicy: k3s/k3d,
// Calico, Cilium, ...). The kubelet's HTTP probes would hit that closed
// port too, so readiness moves to the DLP container, whose /healthz also
// checks the server's health path.
//
// URL Skill (a remote MCP server):
//
//	Service :port ──▶ gateway Pod :18090 (MCP mode) ──▶ https://remote/mcp
//
// The gateway holds the credential (spec.authSecretRef) and drops the
// caller's, and audits every call. With a dlpRef it enforces that policy;
// without one it runs audit-only on the built-in detectors, so traffic to
// a third-party server is at least visible.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// skillDLPPort is where the MCP-mode DLP proxy (or remote gateway)
// listens inside the Skill Pod.
const skillDLPPort = int32(18090)

// skillPort is the port the Skill's MCP server listens on.
func skillPort(s *orchv1alpha1.Skill) int32 {
	if s.Spec.Port != 0 {
		return s.Spec.Port
	}
	return 3000
}

func skillHealthPath(s *orchv1alpha1.Skill) string {
	if s.Spec.HealthPath != "" {
		return s.Spec.HealthPath
	}
	return "/health"
}

// isRemoteSkill reports whether the Skill fronts a remote MCP server.
func isRemoteSkill(s *orchv1alpha1.Skill) bool { return s.Spec.URL != "" }

// validateSkill catches what the CRD's CEL rules catch, for API servers
// too old to run them.
func validateSkill(s *orchv1alpha1.Skill) string {
	switch {
	case s.Spec.Image == "" && s.Spec.URL == "":
		return "set spec.image (run an MCP server) or spec.url (a remote one)"
	case s.Spec.Image != "" && s.Spec.URL != "":
		return "set only one of spec.image and spec.url"
	case s.Spec.AuthSecretRef != nil && s.Spec.URL == "":
		return "spec.authSecretRef is only for remote (url) Skills"
	}
	if s.Spec.URL != "" {
		u, err := url.Parse(s.Spec.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Sprintf("spec.url %q is not an http(s) URL", s.Spec.URL)
		}
	}
	return ""
}

// resolveDLPRef returns the Ready policy a resource refers to. A non-empty
// problem means: don't deploy yet (fail closed). Shared by Models and
// Skills.
func resolveDLPRef(ctx context.Context, c client.Client, ns string, ref *orchv1alpha1.LocalObjectReference, kind string) (*resolvedDLPPolicy, string, error) {
	if ref == nil {
		return nil, "", nil
	}
	if ref.Namespace != "" && ref.Namespace != ns {
		return nil, fmt.Sprintf("dlpRef %s/%s: the DLPPolicy must be in the %s's own namespace (%s)",
			ref.Namespace, ref.Name, kind, ns), nil
	}
	var p orchv1alpha1.DLPPolicy
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &p)
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Sprintf("missing DLPPolicy %s; the %s won't be served without it", ref.Name, kind), nil
	case err != nil:
		return nil, "", fmt.Errorf("get dlppolicy %q: %w", ref.Name, err)
	case p.Status.Phase == orchv1alpha1.DLPPolicyPhaseInvalid:
		return nil, fmt.Sprintf("invalid DLPPolicy %s (see its Validated condition)", ref.Name), nil
	case p.Status.Phase != orchv1alpha1.DLPPolicyPhaseReady:
		return nil, fmt.Sprintf("waiting for DLPPolicy %s to be validated", ref.Name), nil
	}
	return snapshotDLPPolicy(&p), "", nil
}

// observeOnlyPolicy is what a remote Skill's gateway runs without a
// dlpRef: built-in detectors plus global-safe, audit only.
func observeOnlyPolicy() *resolvedDLPPolicy {
	return &resolvedDLPPolicy{
		packs:         []string{"global-safe"},
		defaultAction: orchv1alpha1.DLPActionAudit,
		direction:     orchv1alpha1.DLPDirectionBoth,
		auditPath:     "-",
	}
}

// mcpProxyContainer is the DLP proxy in MCP mode, listening on
// skillDLPPort and probed on its own /healthz.
func mcpProxyContainer(name string, pol *resolvedDLPPolicy, upstream, upstreamHealth string) corev1.Container {
	c := dlpProxyContainer(name, pol, upstream, skillDLPPort)
	setEnv(&c, "BAIRAV_DLP_MODE", "mcp")
	if upstreamHealth != "" {
		setEnv(&c, "BAIRAV_DLP_UPSTREAM_HEALTH", upstreamHealth)
	}
	c.Ports[0].Name = "mcp-dlp"
	c.ReadinessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(skillDLPPort)}},
		PeriodSeconds:    5,
		FailureThreshold: 12,
	}
	return c
}

// withToolPin tells the MCP proxy which Skill it serves (for the
// "bairav skill approve <name>" hint) and the approved tool list, if any.
// A pin change changes the env, so the Pod rolls with the new list.
func withToolPin(c *corev1.Container, s *orchv1alpha1.Skill) {
	setEnv(c, "BAIRAV_SKILL_NAME", s.Name)
	if len(s.Spec.ToolsPin) == 0 {
		return
	}
	b, err := json.Marshal(s.Spec.ToolsPin) // sorted keys: stable
	if err != nil {
		return
	}
	setEnv(c, "BAIRAV_MCP_TOOLS_PIN", string(b))
}

// withSkillDLP puts the MCP-mode proxy in front of an image Skill's server.
func withSkillDLP(d *appsv1.Deployment, pol *resolvedDLPPolicy, s *orchv1alpha1.Skill) {
	spec := &d.Spec.Template.Spec
	if len(spec.Containers) == 0 {
		return
	}
	srv := &spec.Containers[0]
	// The NetworkPolicy closes this port to the kubelet as well, so the
	// DLP container's /healthz checks the server instead.
	srv.ReadinessProbe, srv.LivenessProbe = nil, nil
	upstream := fmt.Sprintf("http://127.0.0.1:%d", skillPort(s))
	c := mcpProxyContainer("dlp", pol, upstream, skillHealthPath(s))
	withToolPin(&c, s)
	spec.Containers = append(spec.Containers, c)
	addPolicyVolumeAndHash(&d.Spec.Template, pol)
}

// addPolicyVolumeAndHash mounts custom rules and stamps the policy hash so
// policy edits roll the Pod.
func addPolicyVolumeAndHash(t *corev1.PodTemplateSpec, pol *resolvedDLPPolicy) {
	if pol.customRulesCount > 0 {
		t.Spec.Volumes = append(t.Spec.Volumes, corev1.Volume{
			Name: dlpPolicyVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: DLPPolicyConfigMapName(pol.name)},
			}},
		})
	}
	if pol.name == "" {
		return
	}
	if t.Annotations == nil {
		t.Annotations = map[string]string{}
	}
	t.Annotations["orch.bairavs.com/dlp-policy"] = pol.name
	t.Annotations["orch.bairavs.com/dlp-policy-hash"] = pol.hash
}

// buildRemoteSkillDeployment is the gateway Pod for a URL Skill.
func buildRemoteSkillDeployment(s *orchv1alpha1.Skill, labels map[string]string, pol *resolvedDLPPolicy) *appsv1.Deployment {
	if pol == nil {
		pol = observeOnlyPolicy()
	}
	// The gateway forwards to the server's origin and callers keep the
	// URL's path: MCP servers hand out absolute paths (the SSE transport's
	// "/messages?session_id=..."), which must land on the same host
	// unchanged, not under the URL's path.
	gw := mcpProxyContainer("gateway", pol, remoteOrigin(s.Spec.URL), "")
	withToolPin(&gw, s)
	if ref := s.Spec.AuthSecretRef; ref != nil {
		gw.Env = append(gw.Env, corev1.EnvVar{Name: "BAIRAV_MCP_UPSTREAM_TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: ref.DeepCopy()}})
	}
	if s.Spec.Resources.Requests != nil || s.Spec.Resources.Limits != nil {
		gw.Resources = *s.Spec.Resources.DeepCopy()
	} else {
		gw.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		}
	}
	one := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{AutomountServiceAccountToken: noServiceAccountToken(), Containers: []corev1.Container{gw}},
			},
		},
	}
	addPolicyVolumeAndHash(&d.Spec.Template, pol)
	return d
}

// buildSkillNetworkPolicy admits traffic to an image Skill's Pod on the
// DLP port only.
func buildSkillNetworkPolicy(s *orchv1alpha1.Skill, podLabels map[string]string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(skillDLPPort)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: skillNetworkPolicyName(s.Name), Namespace: s.Namespace,
			Labels: map[string]string{"orch.bairavs.com/skill": s.Name}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"orch.bairavs.com/skill": podLabels["orch.bairavs.com/skill"]}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}}},
		},
	}
}

func skillNetworkPolicyName(skill string) string { return skill + "-dlp-only" }

// applySkillNetworkPolicy creates/updates the policy when wanted, and
// deletes it when the Skill no longer has a dlpRef.
func (r *SkillReconciler) applySkillNetworkPolicy(ctx context.Context, s *orchv1alpha1.Skill, desired *networkingv1.NetworkPolicy) error {
	var existing networkingv1.NetworkPolicy
	key := types.NamespacedName{Namespace: s.Namespace, Name: skillNetworkPolicyName(s.Name)}
	err := r.Get(ctx, key, &existing)
	switch {
	case desired == nil && apierrors.IsNotFound(err):
		return nil
	case desired == nil && err == nil:
		if !metav1.IsControlledBy(&existing, s) {
			return nil // not ours; leave it
		}
		return client.IgnoreNotFound(r.Delete(ctx, &existing))
	case apierrors.IsNotFound(err):
		return r.Create(ctx, desired)
	case err != nil:
		return fmt.Errorf("get networkpolicy: %w", err)
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

// skillsForDLPPolicy wakes every Skill in the policy's namespace that
// references it.
func (r *SkillReconciler) skillsForDLPPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	var list orchv1alpha1.SkillList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list skills for dlppolicy", "policy", obj.GetName())
		return nil
	}
	var out []reconcile.Request
	for _, s := range list.Items {
		if s.Spec.DLPRef != nil && s.Spec.DLPRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: s.Namespace, Name: s.Name}})
		}
	}
	return out
}

// remotePath is the path part of a remote Skill's URL, appended to the
// in-cluster endpoint so callers hit the same route ("/mcp", "/sse").
func remotePath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "/" {
		return ""
	}
	return u.Path
}

// remoteOrigin is scheme://host[:port] of a remote Skill's URL.
func remoteOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Scheme + "://" + u.Host
}
