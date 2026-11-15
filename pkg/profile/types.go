// Package profile is the settings engine: given a model revision and a request, it picks the GPU
// type and count, tensor-parallel size, dtype and context length, or refuses with a code. It is a
// pure function (no network, no cluster), so it is fully unit-tested. The formula is
// docs/design/profile.md. Spine file (P01 T1): types and constants only; Plan is in profile.go (T3).
package profile

// Constants of the formula in docs/design/profile.md. testdata/spine/profile/expected.py copies
// them; tests/profile-expected.test.sh fails if the two disagree.
const (
	// DefaultGPUMemoryPercent is vLLM's --gpu-memory-utilization (as a percent) when the spec omits it.
	DefaultGPUMemoryPercent = 90
	// ReserveMiB is subtracted from each GPU's budget for activations, CUDA graphs and the CUDA
	// context, on top of the weights. T0 measured about 2.3 GiB for Qwen3-8B on one L4 (vLLM 0.32,
	// CUDA-graph memory profiling on); 2.5 GiB keeps the estimate slightly conservative.
	ReserveMiB = 2560
	// ReserveTPMiB is added to ReserveMiB per GPU when tensor-parallel size > 1. T0 measured the
	// per-GPU reserve *falling* under TP=2 (about 1.3 GiB on 2x L4: activations split too), so no
	// extra is needed; the constant stays so a later calibration can raise it.
	ReserveTPMiB = 0
	// MinContextTokens is the smallest context a profile may offer; below it, try more GPUs.
	MinContextTokens = 4096
	// ContextStepTokens: an automatic max context is rounded down to a multiple of this.
	ContextStepTokens = 1024
	// KVBytesPerElement is the KV-cache element size (16-bit KV cache in v0.1).
	KVBytesPerElement = 2
)

// GPU counts tried, in order.
var GPUCounts = []int{1, 2, 4, 8}

// Catalog is catalog/gpus.yaml.
type Catalog struct {
	GPUs []GPU `json:"gpus"`
}

// GPU is one accelerator type.
type GPU struct {
	Type       string  `json:"type"`
	Deployable bool    `json:"deployable"`
	UsableMiB  int64   `json:"usableMiB"`
	Shapes     []Shape `json:"shapes"`
}

// Shape is the machine for a GPU count, and the pod's CPU and memory requests on it.
type Shape struct {
	GPUs        int    `json:"gpus"`
	MachineType string `json:"machineType"`
	CPU         string `json:"cpu"`
	Memory      string `json:"memory"`
}

// Architectures is the set of text-generation architectures the pinned vLLM supports.
type Architectures map[string]bool

// File is one file in a model revision.
type File struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Model is what the engine knows about one model revision. pkg/hf fills it from the Hub; tests fill
// it from testdata/hf/.
type Model struct {
	Repo        string `json:"repo"`        // "Qwen/Qwen3-8B"
	Revision    string `json:"revision"`    // commit SHA
	PipelineTag string `json:"pipelineTag"` // "" when the Hub has none
	Gated       bool   `json:"gated"`
	License     string `json:"license"`
	Files       []File `json:"files"` // every file in the revision, with sizes
	// IndexWeightFiles are the distinct file names in model.safetensors.index.json's weight_map,
	// sorted; nil when the revision has no index.
	IndexWeightFiles []string `json:"indexWeightFiles"`
	// Config is nil when config.json could not be read (a gated model without a token).
	Config *Config `json:"config"`
}

// Config is the subset of config.json the formula uses. pkg/hf reads each field from the top
// level, falling back to "text_config" when the top level lacks it.
type Config struct {
	Architectures         []string `json:"architectures"`
	HiddenSize            int      `json:"hiddenSize"`
	NumHiddenLayers       int      `json:"numHiddenLayers"`
	NumAttentionHeads     int      `json:"numAttentionHeads"`
	NumKeyValueHeads      int      `json:"numKeyValueHeads"` // 0 when absent: means NumAttentionHeads
	HeadDim               int      `json:"headDim"`          // 0 when absent: means HiddenSize/NumAttentionHeads
	MaxPositionEmbeddings int      `json:"maxPositionEmbeddings"`
	Dtype                 string   `json:"dtype"`         // "dtype", else "torch_dtype"; "" when neither
	KVLoraRank            int      `json:"kvLoraRank"`    // MLA (DeepSeek-style); 0 otherwise
	QKRopeHeadDim         int      `json:"qkRopeHeadDim"` // MLA; 0 otherwise
	QuantMethod           string   `json:"quantMethod"`   // quantization_config.quant_method; "" if none
}

// Request is what the spec constrains. Zero values mean "choose".
type Request struct {
	GPUType          string `json:"gpuType"`          // "auto" or "" = choose; else a catalog type
	GPUCount         int    `json:"gpuCount"`         // 0 = choose
	MaxModelLen      int    `json:"maxModelLen"`      // 0 = choose
	GPUMemoryPercent int    `json:"gpuMemoryPercent"` // 0 = DefaultGPUMemoryPercent
	HasToken         bool   `json:"hasToken"`         // an HF token is available for gated models
}

// Result is a chosen profile.
type Result struct {
	GPUType            string `json:"gpuType"`
	GPUCount           int    `json:"gpuCount"`
	Shape              Shape  `json:"shape"`
	TensorParallelSize int    `json:"tensorParallelSize"`
	Dtype              string `json:"dtype"`
	MaxModelLen        int    `json:"maxModelLen"`
	GPUMemoryPercent   int    `json:"gpuMemoryPercent"`
	// Deployable is false when only a GPU type v0.1 doesn't deploy fits (podserve deploy refuses).
	Deployable bool `json:"deployable"`
	// Estimates, across all GPUs of one replica.
	WeightsBytes    int64 `json:"weightsBytes"`
	KVBytesPerToken int64 `json:"kvBytesPerToken"`
	KVCacheBytes    int64 `json:"kvCacheBytes"`
	TokenCapacity   int64 `json:"tokenCapacity"`
	SeqsAtMaxLen    int64 `json:"seqsAtMaxLen"`
	// Warnings are shown by `podserve plan`, e.g. a missing pipeline_tag.
	Warnings []string `json:"warnings"`
}

// Code is a refusal reason. The CLI's message for each is in docs/design/cli.md.
type Code string

const (
	UnsupportedArchitecture Code = "UNSUPPORTED_ARCHITECTURE"
	NotTextGeneration       Code = "NOT_TEXT_GENERATION"
	NoSafetensors           Code = "NO_SAFETENSORS"
	GatedNoToken            Code = "GATED_NO_TOKEN"
	DoesNotFit              Code = "DOES_NOT_FIT"
	InvalidConfig           Code = "INVALID_CONFIG"
	UnknownGPUType          Code = "UNKNOWN_GPU_TYPE"
)

// Refusal is the error Plan returns when it won't produce a profile.
type Refusal struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (r *Refusal) Error() string { return string(r.Code) + ": " + r.Message }
