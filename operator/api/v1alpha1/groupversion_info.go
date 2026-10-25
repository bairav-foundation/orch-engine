// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains API Schema definitions for the orch.bairavs.com
// v1alpha1 API group: Model, Skill, Agent, DLPPolicy and AccessGrant.
//
// +kubebuilder:object:generate=true
// +groupName=orch.bairavs.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group + version used by all types in this package.
	GroupVersion = schema.GroupVersion{Group: "orch.bairavs.com", Version: "v1alpha1"}

	// SchemeBuilder collects our types; each *_types.go registers in init().
	// Uses only apimachinery, so the API package stays light to import
	// (controller-runtime's scheme.Builder is deprecated for this reason).
	SchemeBuilder = &schemeBuilder{}

	// AddToScheme is the standard convenience handle used in main().
	AddToScheme = SchemeBuilder.AddToScheme
)

type schemeBuilder struct{ objs []runtime.Object }

// Register adds types to be registered under GroupVersion.
func (b *schemeBuilder) Register(objs ...runtime.Object) {
	b.objs = append(b.objs, objs...)
}

// AddToScheme registers all collected types with s, the same way
// controller-runtime's builder did.
func (b *schemeBuilder) AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, b.objs...)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
