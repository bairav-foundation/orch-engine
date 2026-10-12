// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "testing"

func TestAccessGrantAllows(t *testing.T) {
	g := &AccessGrant{Spec: AccessGrantSpec{
		From: []AccessGrantFrom{{Namespace: "team-b"}},
		To: []AccessGrantTo{
			{Kind: AccessGrantKindModel, Name: "qwen"},
			{Kind: AccessGrantKindSkill}, // every Skill
		},
	}}
	cases := []struct {
		from string
		kind AccessGrantKind
		name string
		want bool
	}{
		{"team-b", AccessGrantKindModel, "qwen", true},
		{"team-b", AccessGrantKindModel, "gemma", false}, // model not listed
		{"team-b", AccessGrantKindSkill, "anything", true},
		{"team-c", AccessGrantKindModel, "qwen", false}, // namespace not in From
	}
	for _, c := range cases {
		if got := g.Allows(c.from, c.kind, c.name); got != c.want {
			t.Errorf("Allows(%s, %s, %s) = %v, want %v", c.from, c.kind, c.name, got, c.want)
		}
	}
}
