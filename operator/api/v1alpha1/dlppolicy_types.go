// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DLPPolicyPhase describes the lifecycle of a DLPPolicy CR.
//
// A DLPPolicy is pure config — the reconciler validates it and reports
// Ready or Invalid, but no workloads are created directly. It's the
// Agent CRD (via spec.dlpRef) that actually spawns a DLP sidecar
// wearing this policy.
//
// Pending  — CR observed; reconciler hasn't validated yet.
// Ready    — Spec passed validation; safe to reference from Agents.
// Invalid  — Validation failed; see Conditions for the specific error.
type DLPPolicyPhase string

// DLPPolicyPhase values.
const (
	DLPPolicyPhasePending DLPPolicyPhase = "Pending"
	DLPPolicyPhaseReady   DLPPolicyPhase = "Ready"
	DLPPolicyPhaseInvalid DLPPolicyPhase = "Invalid"
)

// DLPAction mirrors the dlp.Action strings understood by the DLP proxy
// binary: audit / redact / block. Kept as a string alias with an enum
// validation so kubectl/kubeapi rejects typos at admission time.
//
// +kubebuilder:validation:Enum=audit;redact;block
type DLPAction string

// DLPAction values.
const (
	DLPActionAudit  DLPAction = "audit"
	DLPActionRedact DLPAction = "redact"
	DLPActionBlock  DLPAction = "block"
)

// DLPDirection is which side of the Model call this policy inspects.
//
// request  — only inspect Agent → Model (prompt-side; catches secrets
//
//	leaving the perimeter).
//
// response — only inspect Model → Agent (completion-side; catches PII
//
//	in generated output before it reaches the agent code).
//
// both     — inspect both directions. Default. The most useful setting
//
//	for the "governed control plane" pitch.
//
// +kubebuilder:validation:Enum=request;response;both
type DLPDirection string

// DLPDirection values.
const (
	DLPDirectionRequest  DLPDirection = "request"
	DLPDirectionResponse DLPDirection = "response"
	DLPDirectionBoth     DLPDirection = "both"
)

// DLPCustomRule is one inline detection rule, mirroring the shape of
// the on-disk YAML policies (cli/internal/dlp/policy.go's Rule) so
// existing tooling can round-trip between the two.
//
// A rule is a labelled regex + optional checksum validator + optional
// per-rule action override. When multiple rules hit on the same request
// the strongest action wins (block > redact > audit), same as
// the CLI.
type DLPCustomRule struct {
	// Label is the SOC-facing name for the hit type (e.g. "IBAN",
	// "INTERNAL_TICKET_ID"). Uppercase snake case by convention.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Label string `json:"label"`

	// Regex is a Go-flavour RE2 pattern that captures the sensitive
	// span. Anchoring is up to the author; the runtime does not add
	// implicit ^ or $.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Regex string `json:"regex"`

	// Checksum names a built-in validator applied to each regex match
	// before it counts as a hit (e.g. "luhn", "verhoeff", "mod11").
	// Empty means no checksum — every regex match is a hit.
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// Action overrides the DLPPolicy's defaultAction for this specific
	// rule. Useful for the mixed-severity case (redact most PII, but
	// hard-block a specific internal-secret pattern).
	// +optional
	Action DLPAction `json:"action,omitempty"`
}

// DLPPolicySpec is the user-facing knobs of a DLPPolicy CR.
//
// Design shape: the same knobs a user already passes on the CLI
// (`--pack global-safe --policy corp.yaml --action redact`) surfaced as
// a K8s object. That symmetry keeps the mental model constant across
// Compose (flags) and K8s (this CR).
type DLPPolicySpec struct {
	// Packs is the list of embedded pack names (from the go:embed set
	// shipped with the binary — "global-safe", "india", "uk"…). Order
	// is preserved; duplicate labels across packs are resolved with
	// first-write-wins, same as the CLI.
	//
	// Empty is legal — a policy can be entirely custom rules. But at
	// least one of Packs or CustomRules must be non-empty (enforced
	// by the reconciler, not by the schema, so the error message can
	// be specific).
	// +optional
	Packs []string `json:"packs,omitempty"`

	// CustomRules is inline rules — same shape as the on-disk YAML.
	// See DLPCustomRule doc.
	// +optional
	CustomRules []DLPCustomRule `json:"customRules,omitempty"`

	// DefaultAction is what the proxy does when a rule with no action
	// override fires. Defaults to "redact" — the least destructive
	// enforcement mode that still keeps the secret out of the model.
	// +optional
	// +kubebuilder:default=redact
	DefaultAction DLPAction `json:"defaultAction,omitempty"`

	// Direction is which side of the Model call this policy inspects.
	// Defaults to "both".
	// +optional
	// +kubebuilder:default=both
	Direction DLPDirection `json:"direction,omitempty"`

	// AuditPath is where the proxy writes JSONL audit entries inside
	// its container. Defaults to "-" (stdout, picked up by kubectl logs
	// and any container-log shipper the cluster runs). Only override
	// when you're mounting a PVC and want file-backed audit retention.
	// +optional
	// +kubebuilder:default="-"
	AuditPath string `json:"auditPath,omitempty"`
}

// DLPPolicyStatus reports observed state. Minimal by design — a policy
// is config, not a workload.
type DLPPolicyStatus struct {
	// Phase is the coarse lifecycle marker; see DLPPolicyPhase docs.
	// +optional
	Phase DLPPolicyPhase `json:"phase,omitempty"`

	// ObservedGeneration is the last spec generation the reconciler
	// finished validating.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RuleCount is the total number of effective detectors this policy
	// resolves to (packs expanded + custom rules), useful for at-a-glance
	// "did my policy actually load anything" checks in `kubectl get`.
	// +optional
	RuleCount int32 `json:"ruleCount,omitempty"`

	// Conditions are the standard K8s condition stream. Type values:
	//   "Validated" — Spec parsed and every rule compiled OK.
	//   "PackLoaded" — All named packs resolved to embedded content.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=dlp;dlps
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Direction",type="string",JSONPath=".spec.direction"
// +kubebuilder:printcolumn:name="Rules",type="integer",JSONPath=".status.ruleCount"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// DLPPolicy is the schema for a reusable DLP configuration referenced
// by Agents (and, in v1.2, Models) to enforce prompt / response
// scanning without hand-rolled sidecar YAML.
type DLPPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DLPPolicySpec   `json:"spec,omitempty"`
	Status DLPPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DLPPolicyList is the K8s standard list wrapper for DLPPolicy.
type DLPPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DLPPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DLPPolicy{}, &DLPPolicyList{})
}
