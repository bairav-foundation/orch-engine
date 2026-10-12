// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package dashboard serves `bairav dashboard`: a local, read-only web
// console for the Orch Engine graph. The UI is one embedded HTML file; the
// only API is GET /api/snapshot, a JSON document describing everything the
// CLI can see (K8s via the user's kubeconfig, docker via container labels).
//
// The same UI also runs with no server at all: Export writes a single
// self-contained HTML file with scripted demo frames inlined, which is what
// bairavs.com/orch-engine/demo hosts.
package dashboard

// Snapshot is one point-in-time view of the graph. Field names are the
// JSON contract with assets/index.html — change both together.
type Snapshot struct {
	Version     string `json:"version"`
	Context     string `json:"context"`     // kubeconfig context, "" if none
	GeneratedAt string `json:"generatedAt"` // RFC 3339
	Mode        string `json:"mode"`        // "live" | "demo"

	Models      []Node       `json:"models"`
	Skills      []Node       `json:"skills"`
	Agents      []AgentNode  `json:"agents"`
	DLPPolicies []PolicyNode `json:"dlpPolicies"`
	Grants      []GrantNode  `json:"grants"`
	Audit       []AuditEvent `json:"audit"`
	Warnings    []string     `json:"warnings,omitempty"` // e.g. "kubernetes unreachable"
	Caption     string       `json:"caption,omitempty"`  // demo frames only
}

// Node is a Model or Skill.
type Node struct {
	Target    string `json:"target"` // "k8s" | "docker"
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Source    string `json:"source"` // HF repo for models, image for skills
	Phase     string `json:"phase"`
	Endpoint  string `json:"endpoint,omitempty"`
	Transport string `json:"transport,omitempty"` // skills only
	// DLPPolicy is set on Models that enforce a policy inside their own Pod
	// (spec.dlpRef): every caller goes through DLP, sidecar or not.
	DLPPolicy string `json:"dlpPolicy,omitempty"`
}

// AgentNode is an Agent plus its declared and bound refs. Refs use the
// same "name" / "ns/name" format as AgentStatus.BoundModels.
type AgentNode struct {
	Target      string   `json:"target"`
	Namespace   string   `json:"namespace"`
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	Phase       string   `json:"phase"`
	Models      []string `json:"models"`
	Skills      []string `json:"skills"`
	BoundModels []string `json:"boundModels"`
	BoundSkills []string `json:"boundSkills"`
	DLPPolicy   string   `json:"dlpPolicy,omitempty"`
	Message     string   `json:"message,omitempty"` // why it's waiting / degraded
	Endpoint    string   `json:"endpoint,omitempty"`
}

// PolicyNode is a DLPPolicy.
type PolicyNode struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Phase     string   `json:"phase"`
	Direction string   `json:"direction"`
	Action    string   `json:"action"`
	Packs     []string `json:"packs"`
	Rules     int32    `json:"rules"`
	Message   string   `json:"message,omitempty"`
}

// GrantNode is an AccessGrant, flattened for display.
type GrantNode struct {
	Namespace string   `json:"namespace"` // owner
	Name      string   `json:"name"`
	From      []string `json:"from"`
	To        []string `json:"to"` // "Model/qwen", "Skill/*"
}

// AuditEvent is one DLP decision, from a sidecar's JSONL audit stream.
type AuditEvent struct {
	Time           string   `json:"time"`
	Namespace      string   `json:"namespace"`
	Agent          string   `json:"agent"`
	Model          string   `json:"model"`
	Direction      string   `json:"direction"`
	Action         string   `json:"action"`
	Labels         []string `json:"labels"`
	ResponseLabels []string `json:"responseLabels,omitempty"`
	LatencyMS      int64    `json:"latencyMs"`
}

// DemoFrame is one step of the scripted demo: a snapshot plus the caption
// shown over it and how long it stays on screen.
type DemoFrame struct {
	Caption    string   `json:"caption"`
	DurationMS int      `json:"durationMs"`
	Snapshot   Snapshot `json:"snapshot"`
}
