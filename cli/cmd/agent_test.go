// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"reflect"
	"testing"

	orchv1alpha1 "github.com/bairav-foundation/orch-engine/operator/api/v1alpha1"
)

func TestRefsFromNames(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []orchv1alpha1.LocalObjectReference
	}{
		{"nil input", nil, nil},
		{"empty slice", []string{}, nil},
		{"single name", []string{"qwen-tiny"},
			[]orchv1alpha1.LocalObjectReference{{Name: "qwen-tiny"}}},
		{"multiple names", []string{"model-a", "model-b", "model-c"},
			[]orchv1alpha1.LocalObjectReference{
				{Name: "model-a"}, {Name: "model-b"}, {Name: "model-c"},
			}},
		{"whitespace trimmed", []string{"  foo  ", "bar  "},
			[]orchv1alpha1.LocalObjectReference{{Name: "foo"}, {Name: "bar"}}},
		{"blank entries dropped", []string{"foo", "", "  ", "bar"},
			[]orchv1alpha1.LocalObjectReference{{Name: "foo"}, {Name: "bar"}}},
		{"cross-namespace ns/name", []string{"team-a/qwen", "local"},
			[]orchv1alpha1.LocalObjectReference{{Namespace: "team-a", Name: "qwen"}, {Name: "local"}}},
	}
	for _, c := range cases {
		got := refsFromNames(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: refsFromNames(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

func TestBuildAgentCR_SetsCoreFields(t *testing.T) {
	a := buildAgentCR("support-agent", "default", "ghcr.io/myorg/agent:v1",
		"/healthz", 9000, []string{"qwen-tiny"}, []string{"fs", "search"})

	if a.Name != "support-agent" || a.Namespace != "default" {
		t.Errorf("meta: got %s/%s, want default/support-agent", a.Namespace, a.Name)
	}
	if a.Spec.Image != "ghcr.io/myorg/agent:v1" {
		t.Errorf("Image = %q", a.Spec.Image)
	}
	if a.Spec.Port != 9000 || a.Spec.HealthPath != "/healthz" {
		t.Errorf("Port/HealthPath = %d, %q", a.Spec.Port, a.Spec.HealthPath)
	}
	if len(a.Spec.ModelRefs) != 1 || a.Spec.ModelRefs[0].Name != "qwen-tiny" {
		t.Errorf("ModelRefs = %+v", a.Spec.ModelRefs)
	}
	if len(a.Spec.SkillRefs) != 2 ||
		a.Spec.SkillRefs[0].Name != "fs" || a.Spec.SkillRefs[1].Name != "search" {
		t.Errorf("SkillRefs = %+v", a.Spec.SkillRefs)
	}
	// The managed-by label is what `bairav agent list` uses to filter
	// out CRs users created by hand — regression guard.
	if a.Labels["app.kubernetes.io/managed-by"] != "bairav" {
		t.Errorf("managed-by label missing")
	}
}
