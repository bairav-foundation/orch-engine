// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// The attachments policy reaches the DLP container only when it's set, so
// an unchanged policy keeps an unchanged Pod template (no restart).
func TestDLPProxyContainer_Attachments(t *testing.T) {
	base := orchv1alpha1.DLPPolicy{Spec: orchv1alpha1.DLPPolicySpec{Packs: []string{"global-safe"}}}
	p := snapshotDLPPolicy(&base)
	if _, ok := envAsMap(dlpProxyContainer("dlp", p, "http://127.0.0.1:18080", 8080).Env)["BAIRAV_DLP_ATTACHMENTS"]; ok {
		t.Error("attachments unset: BAIRAV_DLP_ATTACHMENTS must not be set")
	}

	block := base
	block.Spec.Attachments = orchv1alpha1.DLPAttachmentsBlock
	env := envAsMap(dlpProxyContainer("dlp", snapshotDLPPolicy(&block), "http://127.0.0.1:18080", 8080).Env)
	if env["BAIRAV_DLP_ATTACHMENTS"] != "block" {
		t.Errorf("BAIRAV_DLP_ATTACHMENTS = %q, want block", env["BAIRAV_DLP_ATTACHMENTS"])
	}
	if DLPPolicyHash(base.Spec) == DLPPolicyHash(block.Spec) {
		t.Error("changing attachments must change the policy hash, so Pods pick it up")
	}
}

func TestBuildDeployment_CloudGatewayAttachments(t *testing.T) {
	r := &ModelReconciler{}
	m := cloudModel("openai-compatible", &orchv1alpha1.ModelCloudSpec{BaseURL: "http://vllm:8000/v1"})
	if _, ok := envMap(r.buildDeployment(m).Spec.Template.Spec.Containers[0])["BAIRAV_DLP_ATTACHMENTS"]; ok {
		t.Error("unset dlpAttachments must not set the env var")
	}
	m.Spec.Cloud.DLPAttachments = "block"
	if got := envMap(r.buildDeployment(m).Spec.Template.Spec.Containers[0])["BAIRAV_DLP_ATTACHMENTS"].Value; got != "block" {
		t.Errorf("BAIRAV_DLP_ATTACHMENTS = %q, want block", got)
	}
}
