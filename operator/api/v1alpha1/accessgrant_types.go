// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AccessGrant lets Agents in other namespaces reference objects in this
// one. It lives in the OWNER's namespace — the side being consumed — the
// same shape as Gateway API's ReferenceGrant.
//
// Who may create AccessGrants is ordinary Kubernetes RBAC: a relaxed
// cluster lets team leads grant, a locked-down one reserves it for
// platform admins. The product stays single-mode.
//
// Pure config: no reconciler. The AgentReconciler reads grants when it
// resolves a cross-namespace ref and Watches them so grant/revoke takes
// effect immediately.

// AccessGrantKind is a referenceable kind. DLPPolicy is deliberately not
// grantable: an Agent's DLP sidecar mounts the policy's ConfigMap, and
// Pods can't mount ConfigMaps from another namespace.
// +kubebuilder:validation:Enum=Model;Skill
type AccessGrantKind string

// AccessGrantKind values.
const (
	AccessGrantKindModel AccessGrantKind = "Model"
	AccessGrantKindSkill AccessGrantKind = "Skill"
)

// AccessGrantFrom names a consumer namespace.
type AccessGrantFrom struct {
	// Namespace whose Agents may reference the targets.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// AccessGrantTo names what may be referenced.
type AccessGrantTo struct {
	// Kind of the target object.
	// +kubebuilder:validation:Required
	Kind AccessGrantKind `json:"kind"`

	// Name of the target object. Empty means every object of Kind in this
	// namespace.
	// +optional
	Name string `json:"name,omitempty"`
}

// AccessGrantSpec is the grant itself: every From may reference every To.
type AccessGrantSpec struct {
	// +kubebuilder:validation:MinItems=1
	From []AccessGrantFrom `json:"from"`
	// +kubebuilder:validation:MinItems=1
	To []AccessGrantTo `json:"to"`
}

// AccessGrantStatus is intentionally empty today; reserved for per-
// consumer usage / rate-limit reporting when that lands.
type AccessGrantStatus struct {
	// ObservedGeneration is reserved for a future reconciler.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=grant;grants

// AccessGrant is the schema for cross-namespace access grants.
type AccessGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AccessGrantSpec   `json:"spec,omitempty"`
	Status AccessGrantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AccessGrantList is the K8s standard list wrapper for AccessGrant.
type AccessGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AccessGrant `json:"items"`
}

// Allows reports whether this grant lets fromNamespace reference the
// object kind/name in the grant's own namespace.
func (in *AccessGrant) Allows(fromNamespace string, kind AccessGrantKind, name string) bool {
	fromOK := false
	for _, f := range in.Spec.From {
		if f.Namespace == fromNamespace {
			fromOK = true
			break
		}
	}
	if !fromOK {
		return false
	}
	for _, t := range in.Spec.To {
		if t.Kind == kind && (t.Name == "" || t.Name == name) {
			return true
		}
	}
	return false
}

func init() {
	SchemeBuilder.Register(&AccessGrant{}, &AccessGrantList{})
}
