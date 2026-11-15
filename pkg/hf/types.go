// Package hf reads model revisions from the Hugging Face Hub into profile.Model. Spine file
// (P01 T1): types and contract only; the client is client.go (T5).
//
// The Hub endpoints used (BaseURL defaults to https://huggingface.co):
//
//	GET {BaseURL}/api/models/{repo}/revision/{revision}?blobs=true
//	    -> sha, siblings[].rfilename, siblings[].size, gated (false | "auto" | "manual"),
//	       pipeline_tag, cardData.license
//	GET {BaseURL}/{repo}/resolve/{sha}/config.json
//	GET {BaseURL}/{repo}/resolve/{sha}/model.safetensors.index.json   (only when listed in siblings)
//
// With a token, every request carries "Authorization: Bearer <token>". Responses are recorded under
// testdata/hf/<org>__<model>/ (info.json, config.json, index.json) for the tests.
package hf

import "errors"

// DefaultBaseURL is the public Hub.
const DefaultBaseURL = "https://huggingface.co"

var (
	// ErrNotFound: the repo or revision doesn't exist (HTTP 404 from the model-info call).
	ErrNotFound = errors.New("model or revision not found on the Hugging Face Hub")
	// ErrAccessDenied: a token was sent but the Hub refused config.json (HTTP 401/403), usually a
	// gated model whose licence the token's owner hasn't accepted.
	ErrAccessDenied = errors.New("the Hugging Face Hub refused access with this token; accept the model's licence on huggingface.co")
)

// Behaviour T5 must implement (pinned by testdata/spine/hf/client_test.go):
//   - Model(ctx, repo, revision) resolves revision to the commit SHA and returns profile.Model.
//   - gated is true for "auto" and "manual", false for false.
//   - Gated and no token: neither config.json nor the index is requested (the Hub would answer 401);
//     Model.Config and Model.IndexWeightFiles are nil, and the settings engine refuses with
//     GATED_NO_TOKEN. Gated with a token and a 401/403 on config.json: ErrAccessDenied.
//   - The index is requested only when model.safetensors.index.json is among the siblings.
//   - Config fields are read from config.json with the alias table below: for each field, the
//     first key present (non-null) at the top level wins; if none is, the same keys are tried inside
//     "text_config". Architectures is read from the top level only. Absent means zero / "".
//
//       Field                  keys, in order
//       HiddenSize             hidden_size, n_embd, d_model
//       NumHiddenLayers        num_hidden_layers, n_layer, num_layers
//       NumAttentionHeads      num_attention_heads, n_head
//       NumKeyValueHeads       num_key_value_heads
//       HeadDim                head_dim
//       MaxPositionEmbeddings  max_position_embeddings, n_positions, max_sequence_length, seq_length
//       Dtype                  dtype, torch_dtype
//       KVLoraRank             kv_lora_rank
//       QKRopeHeadDim          qk_rope_head_dim
//       QuantMethod            quantization_config.quant_method
//
//     (GPT-2-era configs use n_layer/n_head/n_embd/n_positions; vLLM serves them, so podserve reads
//     them.) testdata/spine/profile/expected.py and internal/hffixture implement the same table.
//   - IndexWeightFiles: the sorted, distinct values of the index's weight_map; nil with no index.
//   - Files: every sibling with its size, in the Hub's order.
//   - Any other non-2xx status is an error that includes the status code and URL.
