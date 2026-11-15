// Package v1alpha1 is the ModelService API: what a user asks podserve to serve.
//
// This file is the single source of the spec (D-019). controller-gen generates the CRD
// (config/crd/podserve.dev_modelservices.yaml) and the DeepCopy code from the markers here;
// docs/design/modelservice.md explains each field. Validation beyond the markers lives in
// validation.go (T4). Types only in this file.
//
// +kubebuilder:object:generate=true
// +groupName=podserve.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Group    = "podserve.dev"
	Version  = "v1alpha1"
	Kind     = "ModelService"
	Resource = "modelservices"
)

// GroupVersion is podserve.dev/v1alpha1.
var GroupVersion = schema.GroupVersion{Group: Group, Version: Version}

// GroupVersionResource addresses ModelService objects through the dynamic client.
var GroupVersionResource = GroupVersion.WithResource(Resource)

// GroupVersionKind is the ModelService kind.
var GroupVersionKind = GroupVersion.WithKind(Kind)

// ModelService asks podserve to serve one Hugging Face model behind an OpenAI-compatible API.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=msvc,categories=podserve
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.source`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="GPU",type=string,JSONPath=`.status.profile.gpuType`
// +kubebuilder:printcolumn:name="GPUs",type=integer,JSONPath=`.status.profile.gpuCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ModelService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelServiceSpec   `json:"spec"`
	Status ModelServiceStatus `json:"status,omitempty"`
}

// ModelServiceList is a list of ModelService.
//
// +kubebuilder:object:root=true
type ModelServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelService `json:"items"`
}

// ModelServiceSpec is what the user asks for. Everything except model.source has a default.
type ModelServiceSpec struct {
	// Model is the Hugging Face model to serve.
	Model ModelSource `json:"model"`

	// ServedModelName is the name clients send as `model` and the `model_name` metric label.
	// Defaults to the HF repo id, e.g. "Qwen/Qwen3-8B" (D-017).
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._/-]*$`
	// +optional
	ServedModelName string `json:"servedModelName,omitempty"`

	// Accelerator constrains the GPU choice. Omitted fields are chosen by the settings engine.
	// +optional
	Accelerator AcceleratorSpec `json:"accelerator,omitempty"`

	// Engine tunes the vLLM engine. Omitted fields are chosen by the settings engine.
	// +optional
	Engine EngineSpec `json:"engine,omitempty"`

	// Replicas is the fixed number of serving pods. v0.1 has no autoscaling.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
}

// ModelSource names a model revision.
type ModelSource struct {
	// Source is the model URI. v0.1 accepts hf://ORG/MODEL only.
	// +kubebuilder:validation:MaxLength=200
	// +kubebuilder:validation:Pattern=`^hf://[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`
	Source string `json:"source"`

	// Revision is a branch, tag or commit SHA. podserve resolves it to a commit SHA at deploy time
	// and records that in status.resolvedRevision.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._/-]*$`
	// +kubebuilder:default=main
	// +optional
	Revision string `json:"revision,omitempty"`
}

// AcceleratorSpec constrains which GPUs the model may run on.
type AcceleratorSpec struct {
	// Type is a GPU type from catalog/gpus.yaml, or "auto" to let podserve choose.
	// +kubebuilder:validation:Enum=auto;nvidia-l4;nvidia-tesla-a100;nvidia-a100-80gb;nvidia-h100-80gb
	// +kubebuilder:default=auto
	// +optional
	Type string `json:"type,omitempty"`

	// Count is the number of GPUs per replica; 0 lets podserve choose.
	// +kubebuilder:validation:Enum=0;1;2;4;8
	// +kubebuilder:default=0
	// +optional
	Count int32 `json:"count,omitempty"`

	// Spot asks for spot capacity. Defaults to true.
	// +kubebuilder:default=true
	// +optional
	Spot *bool `json:"spot,omitempty"`
}

// EngineSpec tunes vLLM.
type EngineSpec struct {
	// MaxModelLen caps the context length in tokens; 0 lets podserve choose from the KV-cache room.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1048576
	// +optional
	MaxModelLen int32 `json:"maxModelLen,omitempty"`

	// GPUMemoryPercent is the share of each GPU's memory vLLM may use (vLLM's
	// --gpu-memory-utilization, as a whole percent). An integer, because CRD schemas avoid floats.
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=98
	// +kubebuilder:default=90
	// +optional
	GPUMemoryPercent int32 `json:"gpuMemoryPercent,omitempty"`

	// ExtraArgs are passed to `vllm serve` after podserve's own arguments. Arguments podserve sets
	// itself (model, served name, tensor parallelism, context length, memory, dtype, load format,
	// port) are rejected; see docs/design/modelservice.md.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// Phase is where a ModelService is in its lifecycle.
// +kubebuilder:validation:Enum=Pending;Caching;Deploying;Ready;Failed
type Phase string

const (
	PhasePending   Phase = "Pending"
	PhaseCaching   Phase = "Caching"
	PhaseDeploying Phase = "Deploying"
	PhaseReady     Phase = "Ready"
	PhaseFailed    Phase = "Failed"
)

// ModelServiceStatus is written by podserve (the CLI in v0.1, the operator from v0.2), always
// through the /status subresource.
type ModelServiceStatus struct {
	// Phase is the lifecycle phase.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// ResolvedRevision is the commit SHA spec.model.revision resolved to.
	// +optional
	ResolvedRevision string `json:"resolvedRevision,omitempty"`

	// Profile is the serving profile the settings engine chose.
	// +optional
	Profile *ProfileStatus `json:"profile,omitempty"`

	// Endpoint is the cluster-internal OpenAI base URL, e.g. http://NAME.podserve.svc:8000/v1.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Message is a human-readable note on the current phase, e.g. why it failed.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the metadata.generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ProfileStatus records the chosen profile. Sizes are integers (bytes, tokens), never floats.
type ProfileStatus struct {
	GPUType            string `json:"gpuType"`
	GPUCount           int32  `json:"gpuCount"`
	MachineType        string `json:"machineType"`
	TensorParallelSize int32  `json:"tensorParallelSize"`
	Dtype              string `json:"dtype"`
	MaxModelLen        int32  `json:"maxModelLen"`
	GPUMemoryPercent   int32  `json:"gpuMemoryPercent"`
	// WeightsBytes is the estimated weight memory across all GPUs.
	WeightsBytes int64 `json:"weightsBytes"`
	// KVCacheBytes is the estimated KV-cache room across all GPUs.
	KVCacheBytes int64 `json:"kvCacheBytes"`
	// KVBytesPerToken is the KV-cache cost of one token across all GPUs.
	KVBytesPerToken int64 `json:"kvBytesPerToken"`
	// TokenCapacity is how many tokens of KV cache fit.
	TokenCapacity int64 `json:"tokenCapacity"`
}
