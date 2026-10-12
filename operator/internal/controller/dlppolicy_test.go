// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

// TestValidate_EmptyPolicyIsInvalid — a DLPPolicy with no packs and no
// custom rules can't scan anything, so it fails validation.
func TestValidate_EmptyPolicyIsInvalid(t *testing.T) {
	r := &DLPPolicyReconciler{}
	p := &orchv1alpha1.DLPPolicy{}
	status := r.validate(p)
	if status.Phase != orchv1alpha1.DLPPolicyPhaseInvalid {
		t.Errorf("empty policy should be Invalid, got %q", status.Phase)
	}
	if !containsCondition(status.Conditions, "Validated", "False") {
		t.Errorf("expected Validated=False condition, got %+v", status.Conditions)
	}
}

// TestValidate_KnownPack — a policy referencing only known packs is Ready.
func TestValidate_KnownPack(t *testing.T) {
	r := &DLPPolicyReconciler{}
	p := &orchv1alpha1.DLPPolicy{
		Spec: orchv1alpha1.DLPPolicySpec{
			Packs: []string{"global-safe", "india"},
		},
	}
	status := r.validate(p)
	if status.Phase != orchv1alpha1.DLPPolicyPhaseReady {
		t.Errorf("known packs should be Ready, got %q", status.Phase)
	}
	if status.RuleCount == 0 {
		t.Errorf("RuleCount should be > 0 when packs load")
	}
}

// TestValidate_UnknownPack — unknown pack name → Invalid with a message
// naming the pack that couldn't be found.
func TestValidate_UnknownPack(t *testing.T) {
	r := &DLPPolicyReconciler{}
	p := &orchv1alpha1.DLPPolicy{
		Spec: orchv1alpha1.DLPPolicySpec{
			Packs: []string{"invented-pack"},
		},
	}
	status := r.validate(p)
	if status.Phase != orchv1alpha1.DLPPolicyPhaseInvalid {
		t.Errorf("unknown pack should be Invalid, got %q", status.Phase)
	}
	c := findCondition(status.Conditions, "Validated")
	if c == nil || !strings.Contains(c.Message, "invented-pack") {
		t.Errorf("condition message should name the unknown pack; got %+v", c)
	}
}

// TestValidate_BadRegexAndLabel — customRules with a bad regex or a
// lowercased label should both fail validation.
func TestValidate_BadRegexAndLabel(t *testing.T) {
	r := &DLPPolicyReconciler{}
	p := &orchv1alpha1.DLPPolicy{
		Spec: orchv1alpha1.DLPPolicySpec{
			CustomRules: []orchv1alpha1.DLPCustomRule{
				{Label: "lowercased_name", Regex: `.*`},         // bad label
				{Label: "GOOD", Regex: `(unclosed`},             // bad regex
				{Label: "GOOD2", Regex: `\d+`, Checksum: "xyz"}, // unknown checksum
			},
		},
	}
	status := r.validate(p)
	if status.Phase != orchv1alpha1.DLPPolicyPhaseInvalid {
		t.Fatalf("should be Invalid, got %q", status.Phase)
	}
	c := findCondition(status.Conditions, "Validated")
	if c == nil {
		t.Fatal("no Validated condition")
	}
	for _, needle := range []string{"lowercased_name", "does not compile", "checksum"} {
		if !strings.Contains(c.Message, needle) {
			t.Errorf("expected message to contain %q, got %q", needle, c.Message)
		}
	}
}

// TestValidate_GoodCustomRules — passes cleanly.
func TestValidate_GoodCustomRules(t *testing.T) {
	r := &DLPPolicyReconciler{}
	p := &orchv1alpha1.DLPPolicy{
		Spec: orchv1alpha1.DLPPolicySpec{
			CustomRules: []orchv1alpha1.DLPCustomRule{
				{Label: "INTERNAL_TICKET", Regex: `TKT-\d{6}`},
				{Label: "IBAN", Regex: `[A-Z]{2}\d{2}[A-Z0-9]+`, Action: orchv1alpha1.DLPActionBlock},
			},
		},
	}
	status := r.validate(p)
	if status.Phase != orchv1alpha1.DLPPolicyPhaseReady {
		t.Errorf("good custom rules should be Ready, got %q", status.Phase)
	}
	if status.RuleCount < 2 {
		t.Errorf("RuleCount should count custom rules; got %d", status.RuleCount)
	}
}

// TestValidate_LabelCollisions — custom labels may not shadow built-ins,
// enabled pack rules, or each other (the sidecar would crash on start).
func TestValidate_LabelCollisions(t *testing.T) {
	r := &DLPPolicyReconciler{}
	p := &orchv1alpha1.DLPPolicy{
		Spec: orchv1alpha1.DLPPolicySpec{
			Packs: []string{"india"},
			CustomRules: []orchv1alpha1.DLPCustomRule{
				{Label: "EMAIL", Regex: `x`},
				{Label: "PAN", Regex: `y`},
				{Label: "DUP", Regex: `a`},
				{Label: "DUP", Regex: `b`},
			},
		},
	}
	status := r.validate(p)
	if status.Phase != orchv1alpha1.DLPPolicyPhaseInvalid {
		t.Fatalf("collisions should be Invalid, got %q", status.Phase)
	}
	msg := findCondition(status.Conditions, "Validated").Message
	for _, needle := range []string{"built-in", `pack "india"`, "more than once"} {
		if !strings.Contains(msg, needle) {
			t.Errorf("message missing %q: %s", needle, msg)
		}
	}
}

// containsCondition returns true iff a Condition with the given Type
// AND Status ("True"/"False"/"Unknown") is present.
func containsCondition(conds []metav1.Condition, cType, cStatus string) bool {
	c := findCondition(conds, cType)
	return c != nil && string(c.Status) == cStatus
}

// findCondition returns the first Condition with the given Type, or
// nil if none is present. Tiny helper so test messages can name the
// exact condition that was (or wasn't) set.
func findCondition(conds []metav1.Condition, cType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == cType {
			return &conds[i]
		}
	}
	return nil
}
