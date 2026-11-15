// Package platform holds the cluster-wide settings every podserve command reads, and the fixed
// names and layouts that tie Terraform, the CLI, the fetch job and the serving pods together.
// docs/design/platform.md is the contract; this file is its code. Spine file (P01 T1).
package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Fixed names (D-024). Terraform's Workload Identity grants name these, so they are constants.
const (
	Namespace        = "podserve"
	ConfigMapName    = "podserve-platform"
	FetchSA          = "podserve-fetch" // writes the model bucket, reads the HF-token secret
	ModelSA          = "podserve-model" // reads the model bucket
	FieldManager     = "podserve"       // server-side apply field manager
	MarkerName       = "_PODSERVE_COMPLETE"
	ServingPort      = 8000
	FetchTTLSeconds  = 3600 // a finished fetch Job is deleted an hour later (D-021)
	ManagedByLabel   = "app.kubernetes.io/managed-by"
	ManagedByValue   = "podserve"
	ModelLabel       = "podserve.dev/model"    // the ModelService name
	RevisionLabel    = "podserve.dev/revision" // the commit SHA
	ComponentLabel   = "app.kubernetes.io/component"
	FetchHashLabel   = "podserve.dev/fetch" // the fetch Job's hash, on the Job and its pod
	StorageClassName = "standard-rwo"       // for the fetch Job's generic ephemeral volume
)

// The pinned serving image (D-003): vllm/vllm-openai:v0.32.0, by its multi-arch index digest. It
// needs NVIDIA driver 580+ (CUDA 13.0, compatibility mode off); T0 ran it on GKE's "latest" driver.
const (
	VLLMVersion     = "v0.32.0"
	VLLMImageName   = "vllm/vllm-openai"
	VLLMImageDigest = "sha256:befccf0e7e4089aa23926bb6f887a85c0d46c2a99e6de5c5ffdf5cb7957c5b34"
)

// VLLMImageRef is the serving image pulled through the Artifact Registry Docker Hub mirror, e.g.
// us-central1-docker.pkg.dev/PROJECT/podserve-dockerhub/vllm/vllm-openai@sha256:befccf0e....
func VLLMImageRef(dockerhubMirror string) string {
	return strings.TrimSuffix(dockerhubMirror, "/") + "/" + VLLMImageName + "@" + VLLMImageDigest
}

// ConfigMap keys of podserve/podserve-platform.
const (
	KeyProject    = "project"
	KeyRegion     = "region"
	KeyBucket     = "bucket"
	KeyVLLMImage  = "vllmImage"  // by digest, through the Artifact Registry mirror
	KeyFetchImage = "fetchImage" // by digest, built by `podserve init` (D-023)
	KeyHFSecret   = "hfTokenSecret"
)

// Platform is the parsed podserve/podserve-platform ConfigMap.
type Platform struct {
	Project    string
	Region     string
	Bucket     string
	VLLMImage  string
	FetchImage string
	// HFTokenSecret is the Secret Manager secret's resource name, projects/P/secrets/S (no version).
	HFTokenSecret string
}

// FromData parses the ConfigMap's data. Every key is required.
func FromData(data map[string]string) (Platform, error) {
	p := Platform{
		Project:       data[KeyProject],
		Region:        data[KeyRegion],
		Bucket:        data[KeyBucket],
		VLLMImage:     data[KeyVLLMImage],
		FetchImage:    data[KeyFetchImage],
		HFTokenSecret: data[KeyHFSecret],
	}
	for k, v := range map[string]string{KeyProject: p.Project, KeyRegion: p.Region, KeyBucket: p.Bucket,
		KeyVLLMImage: p.VLLMImage, KeyFetchImage: p.FetchImage, KeyHFSecret: p.HFTokenSecret} {
		if v == "" {
			return Platform{}, fmt.Errorf("ConfigMap %s/%s is missing %q; re-run `podserve init`", Namespace, ConfigMapName, k)
		}
	}
	return p, nil
}

// Data is the inverse of FromData.
func (p Platform) Data() map[string]string {
	return map[string]string{
		KeyProject: p.Project, KeyRegion: p.Region, KeyBucket: p.Bucket,
		KeyVLLMImage: p.VLLMImage, KeyFetchImage: p.FetchImage, KeyHFSecret: p.HFTokenSecret,
	}
}

// ModelPrefix is the GCS object prefix for one revision, with no bucket and no trailing slash:
// models/hf/ORG/MODEL/SHA.
func ModelPrefix(repo, sha string) string {
	return "models/hf/" + repo + "/" + sha
}

// ModelURI is gs://BUCKET/models/hf/ORG/MODEL/SHA/ (trailing slash): vLLM's model argument.
func ModelURI(bucket, repo, sha string) string {
	return "gs://" + bucket + "/" + ModelPrefix(repo, sha) + "/"
}

// FetchHash is the first 12 hex characters of sha256("ORG/MODEL@SHA"). Hashing the repo as well as
// the SHA keeps two repos that share git history (HF duplicates) apart.
func FetchHash(repo, sha string) string {
	sum := sha256.Sum256([]byte(repo + "@" + sha))
	return hex.EncodeToString(sum[:])[:12]
}

// FetchJobName is podserve-fetch-HASH.
func FetchJobName(repo, sha string) string {
	return "podserve-fetch-" + FetchHash(repo, sha)
}

// RepoFromSource turns hf://ORG/MODEL into ORG/MODEL.
func RepoFromSource(source string) (string, error) {
	repo, ok := strings.CutPrefix(source, "hf://")
	if !ok || strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return "", fmt.Errorf("model source %q: want hf://ORG/MODEL", source)
	}
	return repo, nil
}

// FetchFileAllowed is the fetch job's file filter (platform.md): what is copied from a revision.
// Weights are safetensors only (D-015); configs, tokenizers and chat templates come along.
func FetchFileAllowed(name string) bool {
	if strings.Contains(name, "/") { // top-level files only; subfolders (e.g. original/) are not served
		return false
	}
	for _, suffix := range []string{".safetensors", ".json", ".txt", ".model", ".jinja", ".tiktoken"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
