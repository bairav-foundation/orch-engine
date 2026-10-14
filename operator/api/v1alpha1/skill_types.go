// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SkillPhase describes the high-level lifecycle of a Skill.
//
// Pending    — CR observed; reconciler hasn't created the Deployment yet.
// Deploying  — Pod is being scheduled or is starting up.
// Ready      — /health is responding; the Skill's MCP endpoint is live.
// Failed     — Terminal error; see Conditions for the cause.
//
// There's no "Pulling"/"Loading" phase like Model has, because Skills
// aren't downloading anything large at first boot — they're just running
// a container the user built. The lifecycle is standard k8s workload.
type SkillPhase string

// SkillPhase values. See the SkillPhase type doc for what each phase means.
const (
	SkillPhasePending   SkillPhase = "Pending"
	SkillPhaseDeploying SkillPhase = "Deploying"
	SkillPhaseReady     SkillPhase = "Ready"
	SkillPhaseFailed    SkillPhase = "Failed"
)

// SkillTransport is the MCP transport the server speaks. v1 supports HTTP
// and SSE (both HTTP-based, easy to expose via a K8s Service). stdio is
// deferred — stdio MCP servers can't sensibly live behind a Service, so
// they need a different runtime shape (sidecar in the Agent Pod, or a
// stdio-to-SSE adapter). Add DLPModeOn-style enum growth if we need it.
type SkillTransport string

// SkillTransport values. Only HTTP-based transports in v1.
const (
	SkillTransportSSE  SkillTransport = "sse"
	SkillTransportHTTP SkillTransport = "http"
)

// SkillSpec is the user-facing knobs of a Skill CR.
//
// Design note: the shape mirrors ModelSpec deliberately. Same operator
// pattern (Skill → Deployment + Service), same lifecycle knobs, so
// operator wiring and CLI stay symmetric. Adding a fourth or fifth CRD
// later gets cheaper each time.
type SkillSpec struct {
	// Image is the container image ref for the user's MCP server. We do
	// NOT ship the MCP server itself: the user does, in whatever language
	// they want. We just run it. Set exactly one of Image and URL.
	// +optional
	Image string `json:"image,omitempty"`

	// URL is a remote MCP server (http or https), for example
	// https://mcp.example.com/mcp. The operator runs a small gateway Pod
	// (the DLP image in MCP mode) in front of it, so Agents call an
	// in-cluster Service, the credential stays in the gateway, and every
	// call is audited. Set exactly one of Image and URL.
	// +optional
	URL string `json:"url,omitempty"`

	// AuthSecretRef names a Secret key holding a bearer token for a
	// remote (URL) server. The gateway sends it upstream and drops
	// whatever credential the caller sent.
	// +optional
	AuthSecretRef *corev1.SecretKeySelector `json:"authSecretRef,omitempty"`

	// DLPRef enforces a DLPPolicy (same namespace) on this Skill's
	// traffic: tool-call arguments on the way in, tool results on the way
	// out. For an image Skill the proxy runs in the Skill's own Pod and a
	// NetworkPolicy closes the server's own port; for a URL Skill it is
	// the gateway's policy.
	// +optional
	DLPRef *LocalObjectReference `json:"dlpRef,omitempty"`

	// Transport is the MCP transport the server speaks. "sse" (default)
	// or "http". stdio is deferred — see SkillTransport docs.
	// +optional
	// +kubebuilder:default=sse
	// +kubebuilder:validation:Enum=sse;http
	Transport SkillTransport `json:"transport,omitempty"`

	// Port is the port the MCP server listens on inside its container.
	// Defaults to 3000, which is the convention for MCP HTTP/SSE
	// servers. Override for servers that listen elsewhere.
	// +optional
	// +kubebuilder:default=3000
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// HealthPath is the HTTP path the readiness/liveness probes hit.
	// Default "/health". Override for servers with a non-standard
	// health endpoint.
	// +optional
	// +kubebuilder:default="/health"
	HealthPath string `json:"healthPath,omitempty"`

	// Env is pass-through container environment. Common uses: pointing
	// the MCP server at its config file, credentials, upstream API
	// endpoints. Same shape as PodSpec's Container.Env so any k8s user
	// recognises it immediately.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources overrides the default Pod resource requests/limits.
	// Skills are generally light (they proxy tool calls, don't run
	// inference), so defaults are conservative if unset.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Service controls the K8s Service the operator creates for this
	// Skill. Same shape as Model's ServiceSpec so the two are
	// substitutable in tooling.
	// +optional
	Service ModelServiceSpec `json:"service,omitempty"`
}

// SkillStatus reports observed state.
type SkillStatus struct {
	// Phase is the high-level lifecycle marker; see SkillPhase docs.
	// +optional
	Phase SkillPhase `json:"phase,omitempty"`

	// Endpoint is the in-cluster URL callers should use to reach the
	// Skill's MCP endpoint, e.g. "http://my-skill.default.svc.cluster.local:3000".
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ObservedGeneration tracks the .metadata.generation we last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the standard K8s condition stream. Type values:
	//   "Available"   — Pod is running and /health has answered.
	//   "Progressing" — Reconciler is making progress (image pull, start-up).
	//   "Degraded"    — Pod is crash-looping or failing health checks.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=skl;skls
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.image"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Skill is the schema for an MCP server deployed and managed by BAIRAVS.
//
// A Skill packages a user-provided container that speaks MCP (SSE or
// HTTP). The operator reconciles each Skill into a Deployment + Service
// so it becomes a first-class, discoverable, governable K8s citizen.
// We deliberately do NOT write the MCP server — the user does, in any
// language. We provide the substrate.
type Skill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SkillSpec   `json:"spec,omitempty"`
	Status SkillStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SkillList is the K8s standard list wrapper for Skill.
type SkillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Skill `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Skill{}, &SkillList{})
}
