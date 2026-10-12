// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains API Schema definitions for the orch.bairavs.com
// v1alpha1 API group: Model, Skill, Agent, DLPPolicy and AccessGrant.
//
// +kubebuilder:object:generate=true
// +groupName=orch.bairavs.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group + version used by all types in this package.
	GroupVersion = schema.GroupVersion{Group: "orch.bairavs.com", Version: "v1alpha1"}

	// SchemeBuilder registers our types with the Kubernetes runtime scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme is the standard convenience handle used in main().
	AddToScheme = SchemeBuilder.AddToScheme
)
