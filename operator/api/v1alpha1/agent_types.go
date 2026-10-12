// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentPhase describes the high-level lifecycle of an Agent.
//
// Pending          — CR observed; not reconciled yet.
// WaitingForDeps   — Referenced Models or Skills aren't Ready yet. Agent
//
//	isn't rolled until they are, so its Pod doesn't start
//	with stale endpoint env vars.
//
// Deploying        — Dependencies satisfied; Pod is scheduling / starting.
// Ready            — /health responding.
// Failed           — Terminal error; see Conditions for the cause.
//
// The WaitingForDeps phase is the piece Model and Skill don't have.
// Agents pull together a graph — if the graph isn't fully assembled the
// Agent shouldn't come up half-wired.
type AgentPhase string

// AgentPhase values. See the AgentPhase type doc for what each means.
const (
	AgentPhasePending        AgentPhase = "Pending"
	AgentPhaseWaitingForDeps AgentPhase = "WaitingForDeps"
	AgentPhaseDeploying      AgentPhase = "Deploying"
	AgentPhaseReady          AgentPhase = "Ready"
	AgentPhaseFailed         AgentPhase = "Failed"
)

// LocalObjectReference is a reference by name, same-namespace by default.
//
// Day 22: an optional Namespace makes it cross-namespace. A cross-namespace
// ref only resolves when the TARGET namespace holds an AccessGrant that
// lets the Agent's namespace use that object (see accessgrant_types.go).
// The type name is kept for API compatibility.
type LocalObjectReference struct {
	// Name of the referenced object.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the referenced object. Empty = the Agent's own
	// namespace. Not allowed on spec.dlpRef (a Pod can't mount a
	// ConfigMap from another namespace).
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// AgentSpec is the user-facing knobs of an Agent CR.
//
// Think of an Agent as the "runtime shape" for a user-provided container
// that does the actual agent loop. The Agent CR declares: which model
// endpoints the agent should have access to, which MCP tool servers it
// should be able to call, and how much it costs to run. The operator
// wires it up — inject env vars pointing at each dependency, create the
// Pod + Service, wait until dependencies are Ready before rolling.
//
// Deliberate constraint: **we do not write the agent code**. The user
// does, in LangGraph, AutoGen, Anthropic SDK, custom Go, whatever. We
// provide the substrate. This is the same "run, don't author" principle
// as Skill — the runtime pattern that's now proven three times over.
type AgentSpec struct {
	// Image is the container image ref for the user's agent code.
	// Required. The image must expose an HTTP endpoint on Port that
	// responds to HealthPath — otherwise readiness never resolves.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// ModelRefs is the set of Model CRs this Agent will call. Each ref
	// produces an env var in the Pod: BAIRAV_MODEL_<UPPER_NAME>_ENDPOINT
	// = the Model's in-cluster URL. The operator waits until each
	// referenced Model is Ready before rolling the Agent Pod.
	//
	// Empty is legal — some agents don't need a model (e.g. pure
	// tool-orchestration agents that call other agents).
	// +optional
	ModelRefs []LocalObjectReference `json:"modelRefs,omitempty"`

	// SkillRefs is the set of Skill CRs this Agent will invoke. Each
	// ref produces an env var: BAIRAV_SKILL_<UPPER_NAME>_ENDPOINT.
	// Same waiting semantics as ModelRefs.
	// +optional
	SkillRefs []LocalObjectReference `json:"skillRefs,omitempty"`

	// Port is the port the agent's HTTP endpoint listens on inside its
	// container. Defaults to 8000 (arbitrary — pick whatever matches
	// your framework's default: FastAPI/uvicorn uses 8000, Flask 5000,
	// Express 3000). Override to match your image.
	// +optional
	// +kubebuilder:default=8000
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// HealthPath is the readiness/liveness probe path. Default /health.
	// +optional
	// +kubebuilder:default="/health"
	HealthPath string `json:"healthPath,omitempty"`

	// Env is pass-through container environment. Agent-framework-
	// specific config (ANTHROPIC_API_KEY, model choice, temperature,
	// etc.) lives here. The operator's own injected env vars
	// (BAIRAV_MODEL_*, BAIRAV_SKILL_*) come from ModelRefs/SkillRefs
	// and never collide because they use the BAIRAV_ prefix namespace.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources overrides the default Pod resource requests/limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Service controls the K8s Service the operator creates for this
	// Agent. Same shape as Model/Skill so tooling stays symmetric.
	// +optional
	Service ModelServiceSpec `json:"service,omitempty"`

	// DLPRef is an OPTIONAL reference to a DLPPolicy CR in the same
	// namespace. When set, the operator injects one DLP proxy sidecar
	// per ModelRef into this Agent's Pod, wearing the DLPPolicy's
	// packs + custom rules, and rewrites BAIRAV_MODEL_<X>_ENDPOINT env
	// vars to point at the sidecar's localhost address so ALL
	// outbound Model traffic is inspected.
	//
	// The Agent waits until the DLPPolicy is Ready (validated) — same
	// WaitingForDeps semantics as Model + Skill refs. If the policy is
	// Invalid the Agent surfaces the policy's error via a Degraded
	// condition so the user sees why the Pod isn't rolling.
	//
	// Skill traffic is NOT proxied in v1.1; DLP on Skill calls is
	// planned for v1.2 alongside the stdio-transport work.
	// +optional
	DLPRef *LocalObjectReference `json:"dlpRef,omitempty"`
}

// AgentStatus reports observed state.
type AgentStatus struct {
	// Phase is the high-level lifecycle marker; see AgentPhase docs.
	// +optional
	Phase AgentPhase `json:"phase,omitempty"`

	// Endpoint is the in-cluster URL the agent's own HTTP surface
	// listens on, e.g. "http://my-agent.default.svc.cluster.local:8000".
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// BoundModels lists the ModelRefs that resolved successfully at
	// the last reconcile. Useful for debugging (which of my refs is
	// causing WaitingForDeps?) and for auditors (what did this agent
	// actually talk to?).
	// +optional
	BoundModels []string `json:"boundModels,omitempty"`

	// BoundSkills mirrors BoundModels for Skill refs.
	// +optional
	BoundSkills []string `json:"boundSkills,omitempty"`

	// ObservedGeneration tracks the .metadata.generation we last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the standard K8s condition stream. Type values:
	//   "Available"    — Pod is running and /health has answered.
	//   "Progressing"  — Reconciler is making progress (image pull, start-up, dependency wait).
	//   "Degraded"     — Pod is crash-looping or a dependency Model/Skill went NotFound.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=agt;agts
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.image"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Agent is the schema for a user-provided agent container deployed and
// managed by BAIRAVS.
//
// An Agent packages a container the user built (in their agent framework
// of choice) and declares which Models and Skills it needs to reach. The
// operator wires those dependencies as env vars, waits for them to be
// Ready, then rolls the Agent Pod. This is the piece that makes the
// three-layer story ("Model + Skill + user code") a single deployable
// graph.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec,omitempty"`
	Status AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList is the K8s standard list wrapper for Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
