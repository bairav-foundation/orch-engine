// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

// Restricted egress for Agents and Skills (spec.egress.mode: restricted).
//
// One NetworkPolicy per workload, named "<name>-egress", selecting its Pods
// and allowing only:
//   - DNS (UDP/TCP 53), so names still resolve,
//   - the Pods of the Models and Skills it depends on (by namespace + label),
//   - the CIDRs listed in spec.egress.allow (optionally per port).
// Everything else outbound is denied. Ingress is untouched.
//
// NetworkPolicy matches IPs, not hostnames, and is only enforced by a CNI
// that implements it (Calico, Cilium, k3s' default). Both are documented.

import (
	"context"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// egressPeer is an in-cluster dependency the workload may reach: the Pods
// labelled labelKey=labelValue in namespace.
type egressPeer struct {
	namespace  string
	labelKey   string
	labelValue string
}

func egressPolicyName(owner string) string { return owner + "-egress" }

// nsNameLabel is set by Kubernetes on every Namespace (1.22+).
const nsNameLabel = "kubernetes.io/metadata.name"

// buildEgressPolicy returns the NetworkPolicy for restricted egress, or nil
// when spec doesn't ask for it. Invalid CIDRs (the CRD validates them, so
// only hand-edited objects) are skipped and returned in the second value
// so the caller can report them.
func buildEgressPolicy(name, namespace string, podLabels map[string]string, labelKey string,
	spec *orchv1alpha1.EgressSpec, peers []egressPeer) (*networkingv1.NetworkPolicy, []string) {
	if !spec.Restricted() {
		return nil, nil
	}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dns := intstr.FromInt32(53)
	rules := []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
	}}
	for _, p := range peers {
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{nsNameLabel: p.namespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{p.labelKey: p.labelValue}},
		}}})
	}
	var bad []string
	for _, a := range spec.Allow {
		if _, _, err := net.ParseCIDR(a.CIDR); err != nil {
			bad = append(bad, a.CIDR)
			continue
		}
		rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: a.CIDR}}}}
		for _, port := range a.Ports {
			pt := intstr.FromInt32(port)
			rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &pt})
		}
		rules = append(rules, rule)
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: egressPolicyName(name), Namespace: namespace,
			Labels: map[string]string{labelKey: name, "app.kubernetes.io/managed-by": "bairav-operator"}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelKey: podLabels[labelKey]}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      rules,
		},
	}, bad
}

// applyEgressPolicy creates or updates desired, or deletes our policy when
// desired is nil (egress back to open). owner must already be set on
// desired as controller.
func applyEgressPolicy(ctx context.Context, c client.Client, owner client.Object, desired *networkingv1.NetworkPolicy) error {
	var existing networkingv1.NetworkPolicy
	key := types.NamespacedName{Namespace: owner.GetNamespace(), Name: egressPolicyName(owner.GetName())}
	err := c.Get(ctx, key, &existing)
	switch {
	case desired == nil && apierrors.IsNotFound(err):
		return nil
	case desired == nil && err == nil:
		if !metav1.IsControlledBy(&existing, owner) {
			return nil // not ours; leave it
		}
		return client.IgnoreNotFound(c.Delete(ctx, &existing))
	case apierrors.IsNotFound(err):
		return c.Create(ctx, desired)
	case err != nil:
		return fmt.Errorf("get egress networkpolicy: %w", err)
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return c.Update(ctx, &existing)
}

// noServiceAccountToken: workload Pods don't call the Kubernetes API, so
// they don't get a token (a stolen token is a common way into a cluster).
// A Pod that really needs one can't get it from these specs today; that
// is deliberate for v0.2.x.
func noServiceAccountToken() *bool { no := false; return &no }
