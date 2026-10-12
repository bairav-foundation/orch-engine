// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// makeFixtureGraph builds a small k8sGraph that exercises every node
// shape, one bound and one unbound edge, and non-alphanumeric names
// that must survive sanitizeID.
func makeFixtureGraph() k8sGraph {
	return k8sGraph{
		models: []k8sRow{
			{namespace: "default", name: "qwen-tiny", repo: "Qwen/Qwen2.5-0.5B-Instruct-GGUF", phase: "Ready", endpoint: "http://qwen.default.svc:8080"},
		},
		skills: []skillRow{
			{namespace: "default", name: "fs", image: "ghcr.io/x/mcp-filesystem:v1", phase: "Ready", endpoint: "http://fs.default.svc:3000"},
			{namespace: "default", name: "search", image: "ghcr.io/x/mcp-search:v1", phase: "Progressing", endpoint: "—"},
		},
		agents: []agentRow{
			{
				namespace:      "default",
				name:           "support-agent",
				image:          "ghcr.io/myorg/agent:v1",
				phase:          "Ready",
				declaredModels: []string{"qwen-tiny"},
				declaredSkills: []string{"fs", "search"},
				boundModels:    "qwen-tiny",
				// search hasn't bound yet — should render as dashed
				boundSkills: "fs",
				endpoint:    "http://support-agent.default.svc:9000",
			},
		},
	}
}

func TestRenderMermaid_ContainsAllNodesAndEdges(t *testing.T) {
	var buf bytes.Buffer
	if err := renderMermaid(&buf, makeFixtureGraph(), "kind-bairavs"); err != nil {
		t.Fatalf("renderMermaid: %v", err)
	}
	out := buf.String()

	// Header names the cluster so operators can tell one paste from another.
	if !strings.Contains(out, "cluster: kind-bairavs") {
		t.Errorf("missing cluster context in header:\n%s", out)
	}
	// One subgraph per namespace.
	if !strings.Contains(out, "subgraph ns_default") {
		t.Errorf("missing default namespace subgraph:\n%s", out)
	}
	// Three node shapes, one per resource type.
	if !strings.Contains(out, "M_default_qwen_tiny[[") {
		t.Errorf("Model node missing or wrong shape:\n%s", out)
	}
	if !strings.Contains(out, "S_default_fs[") {
		t.Errorf("Skill node missing or wrong shape:\n%s", out)
	}
	if !strings.Contains(out, "A_default_support_agent((") {
		t.Errorf("Agent node missing or wrong shape:\n%s", out)
	}
	// Bound edge is solid.
	if !strings.Contains(out, "A_default_support_agent --> M_default_qwen_tiny") {
		t.Errorf("expected solid Agent->Model edge:\n%s", out)
	}
	// Unbound edge (search) is dashed.
	if !strings.Contains(out, "A_default_support_agent -.-> S_default_search") {
		t.Errorf("expected dashed Agent->Skill edge for unbound search:\n%s", out)
	}
	// Bound skill edge is solid.
	if !strings.Contains(out, "A_default_support_agent --> S_default_fs") {
		t.Errorf("expected solid Agent->Skill edge for bound fs:\n%s", out)
	}
	// classDef palette is emitted so the graph actually renders in colour.
	if !strings.Contains(out, "classDef ready") {
		t.Errorf("missing classDef ready:\n%s", out)
	}
}

func TestRenderDOT_ContainsAllNodesAndEdges(t *testing.T) {
	var buf bytes.Buffer
	if err := renderDOT(&buf, makeFixtureGraph(), "kind-bairavs"); err != nil {
		t.Fatalf("renderDOT: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "digraph bairavs") {
		t.Errorf("missing digraph header:\n%s", out)
	}
	if !strings.Contains(out, "subgraph cluster_default") {
		t.Errorf("missing namespace cluster:\n%s", out)
	}
	if !strings.Contains(out, "\"A_default_support_agent\" -> \"M_default_qwen_tiny\" [style=solid]") {
		t.Errorf("expected solid Model edge in DOT:\n%s", out)
	}
	if !strings.Contains(out, "\"A_default_support_agent\" -> \"S_default_search\" [style=dashed]") {
		t.Errorf("expected dashed Skill edge for unbound search in DOT:\n%s", out)
	}
}

func TestSanitizeID_CollapsesNonAlnum(t *testing.T) {
	cases := map[string]string{
		"default":      "default",
		"qwen-tiny":    "qwen_tiny",
		"a.b.c":        "a_b_c",
		"team/prod":    "team_prod",
		"weird!@#name": "weird___name",
	}
	for in, want := range cases {
		if got := sanitizeID(in); got != want {
			t.Errorf("sanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitCSV_HandlesDashAndEmpty(t *testing.T) {
	if got := splitCSV(""); got != nil {
		t.Errorf(`splitCSV("") = %v, want nil`, got)
	}
	if got := splitCSV("—"); got != nil {
		t.Errorf(`splitCSV("—") = %v, want nil`, got)
	}
	got := splitCSV("a, b ,c")
	want := []string{"a", "b", "c"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("splitCSV(\"a, b ,c\") = %v, want %v", got, want)
	}
}

func TestMermaidClass_Mapping(t *testing.T) {
	cases := map[string]string{
		"Ready":          "ready",
		"Progressing":    "progressing",
		"Pulling":        "progressing",
		"WaitingForDeps": "waiting",
		"Pending":        "waiting",
		"Degraded":       "degraded",
		"Failed":         "degraded",
		"":               "waiting", // unknown / empty falls through to waiting
		"WhoKnows":       "waiting",
	}
	for phase, want := range cases {
		if got := mermaidClass(phase); got != want {
			t.Errorf("mermaidClass(%q) = %q, want %q", phase, got, want)
		}
	}
}
