// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// DLPOCRImage runs `bairav dlp ocr` with Tesseract, for DLP policies with
// attachments: scan. Released installs set BAIRAV_DLP_OCR_IMAGE to
// ghcr.io/bairav-foundation/dlp-ocr:<version>.
var DLPOCRImage = envOr("BAIRAV_DLP_OCR_IMAGE", "bairav/dlp-ocr:laptop")

const (
	ocrUID           int64 = 10003 // images/dlp-ocr: useradd -u 10003 ocr
	ocrContainerName       = "dlp-ocr"
)

// ocrContainer is the OCR server next to the DLP proxy (or proxies) in a
// Pod. It listens on loopback only: the proxy in the same Pod is its one
// client, and images never leave the Pod. No probe: the kubelet can't
// reach loopback, and if OCR is down the proxy fails closed (blocks the
// image) rather than letting it through.
func ocrContainer() corev1.Container {
	return corev1.Container{
		Name:            ocrContainerName,
		Image:           DLPOCRImage,
		Env:             []corev1.EnvVar{{Name: "BAIRAV_OCR_LISTEN", Value: "127.0.0.1:8095"}},
		SecurityContext: hardenedSecurityContext(ocrUID, false), // Tesseract writes temp files
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		},
	}
}

// withOCR adds the OCR container to a Pod's containers once.
func withOCR(cs []corev1.Container) []corev1.Container {
	for _, c := range cs {
		if c.Name == ocrContainerName {
			return cs
		}
	}
	return append(cs, ocrContainer())
}
