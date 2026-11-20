package v1alpha1

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// FieldError is one invalid field. Path uses the CRD's JSON path form, e.g. "spec.model.source".
//
// Every non-API type in this package needs the marker below, or controller-gen (run by
// tests/lint.sh) generates DeepCopy code for it and the committed zz_generated file goes stale.
//
// +kubebuilder:object:generate=false
type FieldError struct {
	Path    string
	Message string
}

func (e FieldError) Error() string { return e.Path + ": " + e.Message }

var (
	// nameRe is a DNS-1035 label (docs/design/modelservice.md caps it at 40 characters, because
	// the name is reused for the Deployment and Service).
	nameRe = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
	// sourceRe is hf://ORG/MODEL (v0.1 accepts Hugging Face repos only).
	sourceRe = regexp.MustCompile(`^hf://[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// idRe is a HF repo id, branch, tag or commit SHA.
	idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// gpuTypes is spec.accelerator.type's enum, from catalog/gpus.yaml plus "auto".
var gpuTypes = []string{"auto", "nvidia-l4", "nvidia-tesla-a100", "nvidia-a100-80gb", "nvidia-h100-80gb"}

// ownedArgs are the `vllm serve` arguments podserve sets itself; extraArgs may not set them, in
// any form vLLM's parser accepts (docs/design/modelservice.md, "Validation beyond the schema",
// rule 4).
var ownedArgs = map[string]bool{
	"--model": true, "--served-model-name": true,
	"--tensor-parallel-size": true, "-tp": true,
	"--max-model-len": true, "--gpu-memory-utilization": true, "--dtype": true,
	"--load-format": true, "--model-loader-extra-config": true,
	"--port": true, "--host": true, "--download-dir": true, "--config": true,
}

// ownedList is ownedArgs sorted. The abbreviation search wants the shortest owned argument a
// token is a prefix of, alphabetically first on a tie; scanning in order and keeping a strictly
// shorter match does exactly that.
var ownedList = func() []string {
	list := make([]string, 0, len(ownedArgs))
	for arg := range ownedArgs {
		list = append(list, arg)
	}
	sort.Strings(list)
	return list
}()

// Validate checks a defaulted ModelService against docs/design/modelservice.md and returns every
// problem found, in field order. nil means valid.
func Validate(ms *ModelService) []FieldError {
	var errs []FieldError

	if ms.Name == "" {
		errs = append(errs, FieldError{Path: "metadata.name", Message: "required"})
	} else if len(ms.Name) > 40 || !nameRe.MatchString(ms.Name) {
		errs = append(errs, FieldError{Path: "metadata.name", Message: "must be a lowercase DNS label of at most 40 characters (a-z, 0-9, '-', starting with a letter)"})
	}
	if ms.Namespace != "" && ms.Namespace != "podserve" {
		errs = append(errs, FieldError{Path: "metadata.namespace", Message: `must be "podserve" (podserve v0.1 uses one namespace)`})
	}

	switch src := ms.Spec.Model.Source; {
	case src == "":
		errs = append(errs, FieldError{Path: "spec.model.source", Message: "required"})
	case len(src) > 200:
		errs = append(errs, FieldError{Path: "spec.model.source", Message: "must be at most 200 characters"})
	case !sourceRe.MatchString(src):
		errs = append(errs, FieldError{Path: "spec.model.source", Message: "must be hf://ORG/MODEL"})
	}
	if rev := ms.Spec.Model.Revision; rev != "" && (len(rev) > 128 || !idRe.MatchString(rev)) {
		errs = append(errs, FieldError{Path: "spec.model.revision", Message: "must be a branch, tag or commit SHA (letters, digits, '.', '_', '-', '/'), at most 128 characters"})
	}
	if name := ms.Spec.ServedModelName; name != "" && (len(name) > 128 || !idRe.MatchString(name)) {
		errs = append(errs, FieldError{Path: "spec.servedModelName", Message: "must match [A-Za-z0-9][A-Za-z0-9._/-]*, at most 128 characters"})
	}

	if !gpuTypeAllowed(ms.Spec.Accelerator.Type) {
		errs = append(errs, FieldError{Path: "spec.accelerator.type", Message: "must be one of " + strings.Join(gpuTypes, ", ")})
	}
	switch count := ms.Spec.Accelerator.Count; {
	case count != 0 && count != 1 && count != 2 && count != 4 && count != 8:
		errs = append(errs, FieldError{Path: "spec.accelerator.count", Message: "must be one of 0, 1, 2, 4, 8"})
	case count != 0 && (ms.Spec.Accelerator.Type == "" || ms.Spec.Accelerator.Type == "auto"):
		errs = append(errs, FieldError{Path: "spec.accelerator.count", Message: "needs spec.accelerator.type set (not auto)"})
	}

	switch n := ms.Spec.Engine.MaxModelLen; {
	case n < 0 || n > 1048576:
		errs = append(errs, FieldError{Path: "spec.engine.maxModelLen", Message: "must be between 0 and 1048576"})
	case n != 0 && n < 256:
		errs = append(errs, FieldError{Path: "spec.engine.maxModelLen", Message: "must be 0 (choose) or at least 256"})
	}
	if mem := ms.Spec.Engine.GPUMemoryPercent; mem < 50 || mem > 98 {
		errs = append(errs, FieldError{Path: "spec.engine.gpuMemoryPercent", Message: "must be between 50 and 98"})
	}
	if args := ms.Spec.Engine.ExtraArgs; len(args) > 32 {
		errs = append(errs, FieldError{Path: "spec.engine.extraArgs", Message: "at most 32 items"})
	} else {
		for i, arg := range args {
			if token := firstToken(arg); token != "" {
				if msg := ownedMessage(token); msg != "" {
					errs = append(errs, FieldError{
						Path:    fmt.Sprintf("spec.engine.extraArgs[%d]", i),
						Message: msg,
					})
				}
			}
		}
	}

	if r := ms.Spec.Replicas; r != nil && (*r < 1 || *r > 8) {
		errs = append(errs, FieldError{Path: "spec.replicas", Message: "must be between 1 and 8"})
	}
	return errs
}

// gpuTypeAllowed reports whether t is unset, "auto", or a GPU type from catalog/gpus.yaml.
func gpuTypeAllowed(t string) bool {
	if t == "" {
		return true
	}
	for _, allowed := range gpuTypes {
		if t == allowed {
			return true
		}
	}
	return false
}

// firstToken is an extraArg's flag token, reduced the way docs/design/modelservice.md
// ("Validation beyond the schema", rule 4) reduces it: trimmed, then cut at the first "=", "." or
// whitespace character, whichever comes first. "" means the item is a value (it doesn't start
// with "-"), which is never refused.
func firstToken(arg string) string {
	token := strings.TrimSpace(arg)
	if !strings.HasPrefix(token, "-") {
		return ""
	}
	if end := strings.IndexFunc(token, func(r rune) bool {
		return r == '=' || r == '.' || unicode.IsSpace(r)
	}); end >= 0 {
		token = token[:end]
	}
	return token
}

// ownedMessage is the FieldError message for an extraArg's flag token: "sets <TOKEN>, ..." when
// the token is an owned argument ("_" counts as "-" in "--" tokens, which vLLM's parser converts;
// <TOKEN> is the token as written), "abbreviates <ARG>, ..." when it is a proper prefix of one
// (argparse accepts abbreviations; <ARG> is the shortest owned argument it is a prefix of,
// alphabetically first on a tie), and "" when the token is allowed. "-" and "--" alone are never
// refused.
func ownedMessage(token string) string {
	if token == "-" || token == "--" {
		return ""
	}
	norm := token
	if strings.HasPrefix(token, "--") {
		norm = strings.ReplaceAll(norm, "_", "-")
	}
	if ownedArgs[norm] {
		return "sets " + token + ", which podserve owns"
	}
	by := ""
	for _, arg := range ownedList {
		if strings.HasPrefix(arg, norm) && (by == "" || len(arg) < len(by)) {
			by = arg
		}
	}
	if by == "" {
		return ""
	}
	return "abbreviates " + by + ", which podserve owns"
}
