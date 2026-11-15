// Package catalog embeds podserve's data files into the binary: the GPU catalogue and the list of
// text-generation architectures the pinned vLLM release supports. Spine file (P01 T1).
//
// Regenerate the architecture list with hack/vllm-source.sh and hack/vllm-archs.py when the
// pinned vLLM version changes (see docs/design/profile.md).
package catalog

import _ "embed"

// VLLMVersion is the vLLM release podserve v0.1 pins (D-003).
const VLLMVersion = "v0.32.0"

// GPUsYAML is catalog/gpus.yaml: GPU types, usable memory and machine shapes, in preference order.
//
//go:embed gpus.yaml
var GPUsYAML []byte

// VLLMArchitectures is catalog/vllm-v0.32.0-architectures.txt: one architecture per line, from
// vLLM's _TEXT_GENERATION_MODELS table only.
//
//go:embed vllm-v0.32.0-architectures.txt
var VLLMArchitectures []byte
