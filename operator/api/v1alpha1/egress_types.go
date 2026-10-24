// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// EgressMode says whether the operator limits where a workload's Pods may
// connect to.
// +kubebuilder:validation:Enum=open;restricted
type EgressMode string

const (
	// EgressOpen leaves outbound traffic alone (no NetworkPolicy). Default.
	EgressOpen EgressMode = "open"
	// EgressRestricted allows only DNS, the workload's own dependencies
	// (the Models and Skills it references) and the destinations listed
	// in Allow. Everything else is denied by a NetworkPolicy. Needs a
	// CNI that enforces NetworkPolicy (Calico, Cilium, the k3s default).
	EgressRestricted EgressMode = "restricted"
)

// EgressSpec limits outbound connections from a workload's Pods, so a
// compromised agent or tool can't send data or credentials anywhere it
// likes.
type EgressSpec struct {
	// Mode is "open" (default) or "restricted".
	// +optional
	// +kubebuilder:default=open
	Mode EgressMode `json:"mode,omitempty"`

	// Allow lists extra destinations for restricted mode, by IP range.
	// Kubernetes NetworkPolicy works on IPs, not hostnames: for a SaaS API
	// use its published ranges, or route it through an egress proxy and
	// allow the proxy.
	// +optional
	Allow []EgressRule `json:"allow,omitempty"`
}

// EgressRule is one allowed destination.
type EgressRule struct {
	// CIDR is the destination range, e.g. "10.20.0.0/16" or "203.0.113.7/32".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Format=cidr
	CIDR string `json:"cidr"`

	// Ports limits the rule to these TCP ports. Empty = all ports.
	// +optional
	Ports []int32 `json:"ports,omitempty"`
}

// Restricted reports whether e asks for restricted egress (nil-safe).
func (in *EgressSpec) Restricted() bool {
	return in != nil && in.Mode == EgressRestricted
}
