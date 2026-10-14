// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

// Model-side DLP (spec.dlpRef on a Model).
//
// The Agent sidecar only covers callers that use it: anything else in the
// cluster, or an Agent that ignores its configured endpoint, can reach the
// model's Service directly. With spec.dlpRef the proxy moves into the
// Model's own Pod instead:
//
//	Service :8080 ──▶ dlp container :8080 ──▶ 127.0.0.1:18080 serving
//
// The serving container binds to loopback only, so the DLP container is
// the one way in. The Service, its port and the status endpoint don't
// change, so callers need no changes either.

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// servingLoopbackPort is where llama-server listens (127.0.0.1 only) when
// DLP runs in the Model Pod. Out of the way of the usual service ports.
const servingLoopbackPort = int32(18080)

// modelPort is the Service / client-facing port of a Model.
func modelPort(m *orchv1alpha1.Model) int32 {
	if m.Spec.Service.Port != 0 {
		return m.Spec.Service.Port
	}
	return 8080
}

// snapshotDLPPolicy normalises a Ready DLPPolicy into the fields the Pod
// builders need, with defaults applied. Shared by Agents and Models.
func snapshotDLPPolicy(p *orchv1alpha1.DLPPolicy) *resolvedDLPPolicy {
	action := p.Spec.DefaultAction
	if action == "" {
		action = orchv1alpha1.DLPActionRedact
	}
	dir := p.Spec.Direction
	if dir == "" {
		dir = orchv1alpha1.DLPDirectionBoth
	}
	audit := p.Spec.AuditPath
	if audit == "" {
		audit = "-"
	}
	return &resolvedDLPPolicy{
		name:             p.Name,
		packs:            append([]string(nil), p.Spec.Packs...),
		defaultAction:    action,
		direction:        dir,
		auditPath:        audit,
		customRulesCount: len(p.Spec.CustomRules),
		hash:             DLPPolicyHash(p.Spec),
	}
}

// resolveModelDLP returns the policy to enforce for a Model. A non-empty
// problem means the Model must not be deployed yet (fail closed): the
// policy is missing, invalid, not validated yet, or in another namespace.
func (r *ModelReconciler) resolveModelDLP(ctx context.Context, m *orchv1alpha1.Model) (*resolvedDLPPolicy, string, error) {
	return resolveDLPRef(ctx, r.Client, m.Namespace, m.Spec.DLPRef, "Model")
}

// withModelDLP rewrites a local serving Deployment so the DLP proxy owns
// the client-facing port and the serving container is loopback-only.
//
// Probes: the kubelet probes the Pod IP, which the serving container no
// longer listens on, and probing through the proxy would put a health
// check in the audit log every few seconds. So the serving container is
// probed with curl on loopback (the serving image ships curl), and the
// DLP container answers its own /healthz.
func withModelDLP(d *appsv1.Deployment, dlp *resolvedDLPPolicy, port int32) {
	spec := &d.Spec.Template.Spec
	if len(spec.Containers) == 0 {
		return
	}
	srv := &spec.Containers[0]
	srv.Ports = nil // nothing to expose: only the DLP container reaches it
	if !rebindVLLM(srv, servingLoopbackPort) {
		setEnv(srv, "BAIRAV_HOST", "127.0.0.1")
		setEnv(srv, "BAIRAV_PORT", fmt.Sprintf("%d", servingLoopbackPort))
		loopbackHealth := corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
			"curl", "-fsS", "-o", "/dev/null", fmt.Sprintf("http://127.0.0.1:%d/health", servingLoopbackPort),
		}}}
		for _, pr := range []*corev1.Probe{srv.ReadinessProbe, srv.LivenessProbe} {
			if pr != nil {
				pr.ProbeHandler = loopbackHealth
			}
		}
	}

	upstream := fmt.Sprintf("http://127.0.0.1:%d", servingLoopbackPort)
	proxy := dlpProxyContainer("dlp", dlp, upstream, port)
	proxy.ReadinessProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(port)}},
		PeriodSeconds: 5,
	}
	spec.Containers = append(spec.Containers, proxy)

	if dlp.customRulesCount > 0 {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: dlpPolicyVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: DLPPolicyConfigMapName(dlp.name)},
				},
			},
		})
	}
	if d.Spec.Template.Annotations == nil {
		d.Spec.Template.Annotations = map[string]string{}
	}
	// The hash rolls the Pod when the policy changes (ConfigMap edits
	// alone don't restart anything).
	d.Spec.Template.Annotations["orch.bairavs.com/dlp-policy"] = dlp.name
	d.Spec.Template.Annotations["orch.bairavs.com/dlp-policy-hash"] = dlp.hash
}

// setEnv sets (or adds) one env var on a container.
func setEnv(c *corev1.Container, name, value string) {
	for i := range c.Env {
		if c.Env[i].Name == name {
			c.Env[i].Value = value
			c.Env[i].ValueFrom = nil
			return
		}
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: name, Value: value})
}

// modelsForDLPPolicy wakes every Model in the policy's namespace that
// references it, so a policy turning Ready (or changing) rolls them.
func (r *ModelReconciler) modelsForDLPPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	var list orchv1alpha1.ModelList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list models for dlppolicy", "policy", obj.GetName())
		return nil
	}
	var out []reconcile.Request
	for _, m := range list.Items {
		if m.Spec.DLPRef != nil && m.Spec.DLPRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: m.Namespace, Name: m.Name}})
		}
	}
	return out
}
