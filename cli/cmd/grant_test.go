// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"reflect"
	"testing"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func to(kind orchv1alpha1.AccessGrantKind, name string) orchv1alpha1.AccessGrantTo {
	return orchv1alpha1.AccessGrantTo{Kind: kind, Name: name}
}

const (
	km = orchv1alpha1.AccessGrantKindModel
	ks = orchv1alpha1.AccessGrantKindSkill
)

// Re-running the same grant must be a no-op (dedup + stable order).
func TestMergeTargets_DedupAndSorted(t *testing.T) {
	got := mergeTargets(
		[]orchv1alpha1.AccessGrantTo{to(ks, "echo"), to(km, "qwen")},
		[]orchv1alpha1.AccessGrantTo{to(km, "qwen"), to(km, "gemma")},
	)
	want := []orchv1alpha1.AccessGrantTo{to(km, "gemma"), to(km, "qwen"), to(ks, "echo")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRemoveTargets(t *testing.T) {
	got := removeTargets(
		[]orchv1alpha1.AccessGrantTo{to(km, "qwen"), to(ks, "echo")},
		[]orchv1alpha1.AccessGrantTo{to(km, "qwen")},
	)
	if len(got) != 1 || got[0] != to(ks, "echo") {
		t.Errorf("got %v, want [Skill/echo]", got)
	}
}

func TestExampleRef(t *testing.T) {
	if got := exampleRef("team-a", []orchv1alpha1.AccessGrantTo{to(km, ""), to(ks, "echo")}); got != "--skill team-a/echo" {
		t.Errorf("got %q", got)
	}
}
