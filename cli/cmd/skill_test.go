// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import "testing"

func TestSkillNameFromImage(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Normal image refs — take the last path segment, drop the tag.
		{"mcp/filesystem:v1", "filesystem"},
		{"ghcr.io/myorg/customer-lookup:latest", "customer-lookup"},
		{"registry.example.com:5000/foo/bar:2.0.1", "bar"},
		{"foo:tag", "foo"},
		{"just-a-name", "just-a-name"},
		// Digest form (@sha256:...) — stripped before tag detection.
		{"mcp/search@sha256:abc123", "search"},
		{"mcp/search:v1@sha256:abc123", "search"},
		// Underscores collapse to hyphens (DNS-1123 doesn't allow underscore).
		{"foo/bar_baz_qux:v1", "bar-baz-qux"},
		// Uppercase collapses to lowercase.
		{"Foo/Bar:v1", "bar"},
		// Junk / edge cases — must return the fallback rather than "".
		{"", "bairav-skill"},
		{":just-a-tag", "bairav-skill"},
		{"foo/", "bairav-skill"},
	}
	for _, c := range cases {
		got := skillNameFromImage(c.in)
		if got != c.want {
			t.Errorf("skillNameFromImage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSkillNameFromImage_HonoursDNSLimit(t *testing.T) {
	// A pathological image ref whose last segment is >63 chars must be
	// truncated to fit K8s DNS-1123's 63-char cap. Same guarantee as
	// k8sNameFromRepo for Models.
	huge := "org/" + repeatChar("a", 100) + ":v1"
	got := skillNameFromImage(huge)
	if len(got) > 63 {
		t.Errorf("skillNameFromImage produced length %d, want <= 63", len(got))
	}
}

func repeatChar(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func TestRemoteSkillArg(t *testing.T) {
	if !isRemoteSkillArg("https://mcp.example.com/mcp") || isRemoteSkillArg("ghcr.io/org/img:v1") {
		t.Error("isRemoteSkillArg misclassifies")
	}
	if got := skillNameFromURL("https://mcp.Example.com:8443/sse"); got != "mcp-example-com" {
		t.Errorf("skillNameFromURL = %q", got)
	}
}
