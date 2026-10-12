// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// visualizeCmd emits a graph of the deployed BAIRAVS objects in the
// active kubeconfig context, ready to paste into a Mermaid renderer
// (mermaid.live, GitHub markdown, Obsidian, etc.).
//
// This is a **read-only** view. It reuses collectK8sGraph from ps.go
// so the two commands share a single query path and can never disagree
// about what's in the cluster.
//
// Nodes:
//
//	Model  — double-bracket rectangle  M_<name>
//	Skill  — single-bracket rectangle  S_<name>
//	Agent  — circle                    A_<name>
//
// Edges:
//
//	Agent -->  Model / Skill   when the ref appears in the Agent's
//	                           Status.BoundModels / BoundSkills
//	                           (successful resolution — bound)
//	Agent -.-> Model / Skill   when the ref is declared in Spec but
//	                           hasn't resolved yet (dashed — waiting)
//
// classDef colours phase state (Ready = green, Progressing = amber,
// WaitingForDeps = grey, Degraded / Failed = red).
//
// Design note: we emit source text, we do NOT render pixels. Rendering
// is left to whichever tool the user pastes the output into. This keeps
// the binary tiny and the output diffable.
var visualizeCmd = &cobra.Command{
	Use:     "visualize",
	Aliases: []string{"viz", "graph"},
	Short:   "Emit a graph of Models + Skills + Agents from the active cluster",
	Long: `Print the current cluster state as a graph diagram source.

Node shapes: Model = double-bracket rectangle, Skill = single-bracket,
Agent = circle. Edges run Agent → Model / Skill based on the Agent's
declared refs (dashed until they resolve, solid once bound).

The output is source text meant to be pasted into a renderer:
  * Mermaid: https://mermaid.live, GitHub markdown, Obsidian
  * DOT:     Graphviz (dot -Tpng, or online at dreampuf.github.io)

Examples:
  bairav visualize
  bairav visualize --namespace production
  bairav visualize --format dot | dot -Tpng > graph.png`,
	RunE: runVisualize,
}

func init() {
	visualizeCmd.Flags().StringP("namespace", "n", "",
		"limit the graph to one namespace (default: all namespaces)")
	visualizeCmd.Flags().String("format", "mermaid",
		"output format: mermaid | dot")
	rootCmd.AddCommand(visualizeCmd)
}

func runVisualize(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	ns, _ := cmd.Flags().GetString("namespace")
	format, _ := cmd.Flags().GetString("format")

	graph, ctxName, err := collectK8sGraph(ns)
	if err != nil {
		return fmt.Errorf("collect from cluster: %w", err)
	}
	if graph.total() == 0 {
		// Fprintf with escaped %%%% (→ "%%"), not Fprintln: go vet's printf
		// check reads "%% d..." in a Print-style call as a possible "% d"
		// directive and fails `go test`.
		fmt.Fprintf(out, "%%%% no BAIRAVS objects found in the active context\n")
		fmt.Fprintf(out, "%%%% deploy something with `bairav model deploy` / `skill deploy` / `agent deploy` first\n")
		return nil
	}

	switch strings.ToLower(format) {
	case "mermaid", "":
		return renderMermaid(out, graph, ctxName)
	case "dot", "graphviz":
		return renderDOT(out, graph, ctxName)
	default:
		return fmt.Errorf("unknown --format %q (want mermaid|dot)", format)
	}
}

// renderMermaid emits the graph as a Mermaid `graph LR` block. One
// subgraph per namespace so multi-team clusters read cleanly. Nodes
// are grouped Models → Skills → Agents inside each subgraph.
func renderMermaid(out io.Writer, g k8sGraph, ctxName string) error {
	fmt.Fprintf(out, "%%%% BAIRAVS Orch Engine — cluster: %s\n", ctxName)
	fmt.Fprintln(out, "graph LR")

	// Bucket rows by namespace so each ns becomes one subgraph.
	namespaces := collectNamespaces(g)

	for _, ns := range namespaces {
		fmt.Fprintf(out, "    subgraph ns_%s[\"namespace: %s\"]\n", sanitizeID(ns), ns)

		for _, m := range g.models {
			if m.namespace != ns {
				continue
			}
			label := fmt.Sprintf("Model<br/>%s<br/><small>%s</small>", m.name, truncate(m.repo, 40))
			fmt.Fprintf(out, "        %s[[\"%s\"]]:::%s\n",
				mermaidID("M", ns, m.name), label, mermaidClass(m.phase))
		}
		for _, s := range g.skills {
			if s.namespace != ns {
				continue
			}
			label := fmt.Sprintf("Skill<br/>%s<br/><small>%s</small>", s.name, truncate(s.image, 40))
			fmt.Fprintf(out, "        %s[\"%s\"]:::%s\n",
				mermaidID("S", ns, s.name), label, mermaidClass(s.phase))
		}
		for _, a := range g.agents {
			if a.namespace != ns {
				continue
			}
			label := fmt.Sprintf("Agent<br/>%s<br/><small>%s</small>", a.name, truncate(a.image, 40))
			fmt.Fprintf(out, "        %s((\"%s\")):::%s\n",
				mermaidID("A", ns, a.name), label, mermaidClass(a.phase))
		}
		fmt.Fprintln(out, "    end")
	}

	// Edges — emitted outside subgraphs so they render when the Model or
	// Skill lives in a different namespace than the Agent (Day 22 refs).
	for _, a := range g.agents {
		bound := setOf(splitCSV(a.boundModels))
		for _, ref := range a.declaredModels {
			from := mermaidID("A", a.namespace, a.name)
			to := refNodeID("M", a.namespace, ref)
			if bound[ref] {
				fmt.Fprintf(out, "    %s --> %s\n", from, to)
			} else {
				fmt.Fprintf(out, "    %s -.-> %s\n", from, to)
			}
		}
		boundS := setOf(splitCSV(a.boundSkills))
		for _, ref := range a.declaredSkills {
			from := mermaidID("A", a.namespace, a.name)
			to := refNodeID("S", a.namespace, ref)
			if boundS[ref] {
				fmt.Fprintf(out, "    %s --> %s\n", from, to)
			} else {
				fmt.Fprintf(out, "    %s -.-> %s\n", from, to)
			}
		}
	}

	// Phase colour palette. Deliberately muted so the graph reads well
	// in both light and dark Mermaid themes.
	fmt.Fprintln(out, "    classDef ready fill:#c6f6d5,stroke:#22543d,color:#1a202c")
	fmt.Fprintln(out, "    classDef progressing fill:#feebc8,stroke:#7b341e,color:#1a202c")
	fmt.Fprintln(out, "    classDef waiting fill:#e2e8f0,stroke:#2d3748,color:#1a202c")
	fmt.Fprintln(out, "    classDef degraded fill:#fed7d7,stroke:#742a2a,color:#1a202c")
	return nil
}

// renderDOT emits Graphviz DOT. Same graph, different syntax. For folks
// who prefer `dot -Tpng` to Mermaid.
func renderDOT(out io.Writer, g k8sGraph, ctxName string) error {
	fmt.Fprintf(out, "// BAIRAVS Orch Engine — cluster: %s\n", ctxName)
	fmt.Fprintln(out, "digraph bairavs {")
	fmt.Fprintln(out, "    rankdir=LR;")
	fmt.Fprintln(out, "    node [style=filled, fontname=\"Helvetica\"];")

	namespaces := collectNamespaces(g)
	for _, ns := range namespaces {
		fmt.Fprintf(out, "    subgraph cluster_%s {\n", sanitizeID(ns))
		fmt.Fprintf(out, "        label=\"namespace: %s\";\n", ns)
		fmt.Fprintln(out, "        style=dashed;")

		for _, m := range g.models {
			if m.namespace != ns {
				continue
			}
			fmt.Fprintf(out, "        \"%s\" [shape=box3d,label=\"Model\\n%s\\n%s\",%s];\n",
				dotID("M", ns, m.name), m.name, truncate(m.repo, 40), dotFill(m.phase))
		}
		for _, s := range g.skills {
			if s.namespace != ns {
				continue
			}
			fmt.Fprintf(out, "        \"%s\" [shape=box,label=\"Skill\\n%s\\n%s\",%s];\n",
				dotID("S", ns, s.name), s.name, truncate(s.image, 40), dotFill(s.phase))
		}
		for _, a := range g.agents {
			if a.namespace != ns {
				continue
			}
			fmt.Fprintf(out, "        \"%s\" [shape=circle,label=\"Agent\\n%s\\n%s\",%s];\n",
				dotID("A", ns, a.name), a.name, truncate(a.image, 40), dotFill(a.phase))
		}
		fmt.Fprintln(out, "    }")
	}

	for _, a := range g.agents {
		bound := setOf(splitCSV(a.boundModels))
		for _, ref := range a.declaredModels {
			style := "dashed"
			if bound[ref] {
				style = "solid"
			}
			fmt.Fprintf(out, "    \"%s\" -> \"%s\" [style=%s];\n",
				dotID("A", a.namespace, a.name), refNodeID("M", a.namespace, ref), style)
		}
		boundS := setOf(splitCSV(a.boundSkills))
		for _, ref := range a.declaredSkills {
			style := "dashed"
			if boundS[ref] {
				style = "solid"
			}
			fmt.Fprintf(out, "    \"%s\" -> \"%s\" [style=%s];\n",
				dotID("A", a.namespace, a.name), refNodeID("S", a.namespace, ref), style)
		}
	}
	fmt.Fprintln(out, "}")
	return nil
}

// refNodeID turns a declared ref ("qwen", or "team-a/qwen" for a
// cross-namespace ref) into the ID its Model/Skill node was registered
// under. mermaidID and dotID build identical IDs, so one helper serves both.
func refNodeID(prefix, agentNS, ref string) string {
	if ns, name, ok := strings.Cut(ref, "/"); ok {
		return mermaidID(prefix, ns, name)
	}
	return mermaidID(prefix, agentNS, ref)
}

// mermaidID builds a Mermaid-safe node identifier: prefix + namespace +
// name with anything non-alphanumeric collapsed to `_`. Mermaid rejects
// dots and hyphens in raw IDs.
func mermaidID(prefix, ns, name string) string {
	return fmt.Sprintf("%s_%s_%s", prefix, sanitizeID(ns), sanitizeID(name))
}

func dotID(prefix, ns, name string) string {
	return fmt.Sprintf("%s_%s_%s", prefix, sanitizeID(ns), sanitizeID(name))
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// mermaidClass maps a K8s phase string onto our classDef name.
// Keep in sync with the classDef declarations in renderMermaid.
func mermaidClass(phase string) string {
	switch phase {
	case "Ready":
		return "ready"
	case "Progressing", "Pulling", "Building", "Deploying":
		return "progressing"
	case "WaitingForDeps", "Pending":
		return "waiting"
	case "Degraded", "Failed":
		return "degraded"
	default:
		return "waiting"
	}
}

func dotFill(phase string) string {
	switch phase {
	case "Ready":
		return "fillcolor=\"#c6f6d5\""
	case "Progressing", "Pulling", "Building", "Deploying":
		return "fillcolor=\"#feebc8\""
	case "Degraded", "Failed":
		return "fillcolor=\"#fed7d7\""
	default:
		return "fillcolor=\"#e2e8f0\""
	}
}

// collectNamespaces returns the sorted set of namespaces present in g,
// so the diagram is deterministic across runs.
func collectNamespaces(g k8sGraph) []string {
	seen := map[string]struct{}{}
	for _, m := range g.models {
		seen[m.namespace] = struct{}{}
	}
	for _, s := range g.skills {
		seen[s.namespace] = struct{}{}
	}
	for _, a := range g.agents {
		seen[a.namespace] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// splitCSV is the inverse of joinNonEmpty in ps.go. Returns nil for "—"
// or empty (both mean "no bindings yet") so setOf produces an empty set.
func splitCSV(s string) []string {
	if s == "" || s == "—" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func setOf(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max < 4 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
