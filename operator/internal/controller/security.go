// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// UIDs baked into bairav's own images. Set explicitly because both images
// declare USER by name ("bairav", "nonroot"), and the kubelet can only
// enforce runAsNonRoot against a numeric UID.
const (
	servingUID int64 = 10001 // images/serving: useradd -u 10001 bairav
	dlpUID     int64 = 65532 // distroless :nonroot
)

// hardenedSecurityContext is the restricted Pod Security profile for
// containers running bairav's own images (model server, DLP gateway, DLP
// sidecars): non-root, no privilege escalation, no capabilities, default
// seccomp. User-supplied Agent/Skill images are left to their owners.
func hardenedSecurityContext(uid int64, readOnlyRoot bool) *corev1.SecurityContext {
	no, yes := false, true
	return &corev1.SecurityContext{
		RunAsNonRoot:             &yes,
		RunAsUser:                &uid,
		AllowPrivilegeEscalation: &no,
		ReadOnlyRootFilesystem:   &readOnlyRoot,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// servingSecurityContext hardens the model-server container. Our serving
// image gets the full profile (writable root: the entrypoint uses $HOME;
// models live on the emptyDir). A custom spec.image may run as another
// user, so it keeps its own UID and only loses escalation + capabilities.
func servingSecurityContext(image string) *corev1.SecurityContext {
	sc := hardenedSecurityContext(servingUID, false)
	if image != "" && !strings.HasPrefix(image, "bairav/serving") {
		sc.RunAsUser, sc.RunAsNonRoot = nil, nil
	}
	return sc
}
