// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelPhase describes the high-level lifecycle of a Model.
//
// Pending  — CR observed; reconciler hasn't created the Deployment yet.
// Pulling  — pod is up but the GGUF is still downloading from HF.
// Loading  — model file is on disk; llama-server is loading weights into RAM.
// Ready    — /v1/chat/completions is responding.
// Failed   — terminal error; see Conditions for the cause.
type ModelPhase string

// Model lifecycle phases. See the ModelPhase type doc for what each means.
const (
	ModelPhasePending ModelPhase = "Pending"
	ModelPhasePulling ModelPhase = "Pulling"
	ModelPhaseLoading ModelPhase = "Loading"
	ModelPhaseReady   ModelPhase = "Ready"
	ModelPhaseFailed  ModelPhase = "Failed"
)

// ModelSpec is the user-facing knobs of a Model CR.
type ModelSpec struct {
	// Repo is the Hugging Face repo slug, e.g. "Qwen/Qwen2.5-0.5B-Instruct-GGUF".
	// For a cloud provider (see Provider) it is the provider's model id,
	// e.g. "gpt-4.1-mini" or "claude-sonnet-4-5".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=2
	Repo string `json:"repo"`

	// Provider is where the model runs.
	//   local             — default. Pull Repo from Hugging Face, serve it in-cluster.
	//   openai            — OpenAI API.
	//   anthropic         — Anthropic API.
	//   openai-compatible — any OpenAI-style endpoint (Azure OpenAI, Groq,
	//                       Together, a vLLM elsewhere, ...); needs cloud.baseURL.
	// For cloud providers the operator runs a small DLP gateway instead of
	// a model server: it holds the API key, pins the model, and inspects
	// every request. Agents see the same in-cluster endpoint either way.
	// +optional
	// +kubebuilder:default=local
	// +kubebuilder:validation:Enum=local;openai;anthropic;openai-compatible
	Provider string `json:"provider,omitempty"`

	// Cloud configures the gateway for Provider != local.
	// +optional
	Cloud *ModelCloudSpec `json:"cloud,omitempty"`

	// File is an optional path inside the repo. If empty, the entrypoint picks
	// the smallest .gguf — same heuristic as the CLI.
	// +optional
	File string `json:"file,omitempty"`

	// Revision is the git revision to pull from. Defaults to "main".
	// +optional
	// +kubebuilder:default=main
	Revision string `json:"revision,omitempty"`

	// Backend names the inference engine. "llama-server" (default) serves a
	// GGUF on CPU. "vllm" (experimental) serves a safetensors repo on a GPU
	// node with the vLLM OpenAI server; File is ignored, ContextSize maps to
	// --max-model-len, and Resources default to one nvidia.com/gpu.
	// +optional
	// +kubebuilder:default=llama-server
	// +kubebuilder:validation:Enum=llama-server;vllm
	Backend string `json:"backend,omitempty"`

	// Image is the serving container image. Empty means the operator's
	// default (BAIRAV_SERVING_IMAGE on the operator, ghcr.io/bairav-foundation/serving
	// in released installs); override for a different backend or a private registry.
	// +optional
	Image string `json:"image,omitempty"`

	// ContextSize sets the model's context window in tokens. 0 = backend default.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ContextSize int32 `json:"contextSize,omitempty"`

	// Service controls the Service that exposes the model.
	// +optional
	Service ModelServiceSpec `json:"service,omitempty"`

	// Resources overrides the default Pod resource requests/limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// HFTokenSecretRef references a Secret holding HF_TOKEN under .data[Key].
	// Use for gated/private repos.
	// +optional
	HFTokenSecretRef *SecretKeyRef `json:"hfTokenSecretRef,omitempty"`

	// UpgradeStrategy controls how a change to Repo / File / Image is
	// rolled out.
	//   Surge    — default. The new model pod starts next to the old one and
	//              only takes traffic once Ready; the old pod is removed
	//              after. Zero downtime, but needs memory for both models.
	//   Recreate — stop the old pod first, then start the new one. Brief
	//              downtime; for nodes that can't hold two models at once.
	// +optional
	// +kubebuilder:default=Surge
	// +kubebuilder:validation:Enum=Surge;Recreate
	UpgradeStrategy string `json:"upgradeStrategy,omitempty"`

	// DLPRef enforces a DLPPolicy (same namespace) at the model itself.
	// The operator runs the DLP proxy inside this Model's Pod on the
	// Service port and binds the serving container to 127.0.0.1, so every
	// caller goes through DLP, whether or not it has its own sidecar.
	// The Model isn't deployed until the policy is Ready (fail closed).
	// Local models only: cloud models always run behind the DLP gateway.
	// +optional
	DLPRef *LocalObjectReference `json:"dlpRef,omitempty"`
}

// Model backends. See ModelSpec.Backend.
const (
	ModelBackendLlamaServer = "llama-server"
	ModelBackendVLLM        = "vllm"
)

// Model providers. See ModelSpec.Provider.
const (
	ModelProviderLocal            = "local"
	ModelProviderOpenAI           = "openai"
	ModelProviderAnthropic        = "anthropic"
	ModelProviderOpenAICompatible = "openai-compatible"
)

// IsCloud reports whether the Model is served by a cloud provider through
// the DLP gateway rather than by an in-cluster model server.
func (in ModelSpec) IsCloud() bool {
	return in.Provider != "" && in.Provider != ModelProviderLocal
}

// ModelCloudSpec configures the DLP gateway in front of a cloud provider.
type ModelCloudSpec struct {
	// BaseURL overrides the provider's API base URL (required for
	// openai-compatible), e.g. "https://my-azure.openai.azure.com/openai/v1".
	// +optional
	BaseURL string `json:"baseURL,omitempty"`

	// APIKeySecretRef references the Secret holding the provider API key.
	// Only the gateway pod mounts it; Agents never see it.
	// +optional
	APIKeySecretRef *SecretKeyRef `json:"apiKeySecretRef,omitempty"`

	// DLPAction is the gateway's default action on a hit.
	// +optional
	// +kubebuilder:default=redact
	// +kubebuilder:validation:Enum=redact;block;audit_only
	DLPAction string `json:"dlpAction,omitempty"`

	// DLPPacks lists the detector packs the gateway enables.
	// +optional
	DLPPacks []string `json:"dlpPacks,omitempty"`

	// Image overrides the gateway image (default bairav/dlp:laptop).
	// +optional
	Image string `json:"image,omitempty"`
}

// Model upgrade strategies. See ModelSpec.UpgradeStrategy.
const (
	ModelUpgradeSurge    = "Surge"
	ModelUpgradeRecreate = "Recreate"
)

// ModelServiceSpec controls the K8s Service the operator creates for a Model.
type ModelServiceSpec struct {
	// Type is "ClusterIP", "NodePort", or "LoadBalancer". Defaults to ClusterIP.
	// +optional
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	Type corev1.ServiceType `json:"type,omitempty"`

	// Port is the Service port. Defaults to 8080.
	// +optional
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// SecretKeyRef is a single key inside a single Secret.
type SecretKeyRef struct {
	// Name of the Secret in the same namespace as the Model.
	Name string `json:"name"`
	// Key inside the Secret's .data map.
	Key string `json:"key"`
}

// ModelStatus reports observed state.
type ModelStatus struct {
	// Phase is the high-level lifecycle marker; see ModelPhase docs.
	// +optional
	Phase ModelPhase `json:"phase,omitempty"`

	// Endpoint is the in-cluster URL of the served model, e.g.
	// "http://my-model.default.svc.cluster.local:8080/v1".
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ModelFile is the resolved file path inside the container's cache dir.
	// +optional
	ModelFile string `json:"modelFile,omitempty"`

	// ServedRepo / ServedFile record what is actually serving traffic: they
	// are updated only once a rollout has fully completed (all replicas on
	// the new spec and Ready). While an upgrade is in flight they still show
	// the old model, and the "Upgrading" condition is True.
	// +optional
	ServedRepo string `json:"servedRepo,omitempty"`
	// +optional
	ServedFile string `json:"servedFile,omitempty"`

	// ObservedGeneration tracks the .metadata.generation we last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the standard K8s condition stream. Type values:
	//   "Available"   — Pod is running and llama-server has answered /health
	//   "Progressing" — reconciler is making progress (pulling, loading)
	//   "Degraded"    — Pod is crash-looping or repeatedly failing health checks
	//   "Upgrading"   — a new spec is rolling out; old pod still serving
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mdl;mdls
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Provider",type="string",JSONPath=".spec.provider"
// +kubebuilder:printcolumn:name="Repo",type="string",JSONPath=".spec.repo"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Model is the schema for a deployed LLM in a BAIRAVS-managed cluster.
//
// Each Model maps to one Deployment (running bairav/serving:laptop) and one
// Service (exposing the OpenAI-compatible HTTP endpoint).
type Model struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelSpec   `json:"spec,omitempty"`
	Status ModelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelList is the K8s standard list wrapper for Model.
type ModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Model `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Model{}, &ModelList{})
}
