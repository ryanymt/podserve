package v1alpha1

import "strings"

// Default fills every omitted field with its documented default (docs/design/modelservice.md, the
// same values the CRD's defaults apply). Fields the user set are never overwritten.
func Default(ms *ModelService) {
	if ms.Namespace == "" {
		ms.Namespace = "podserve"
	}
	if ms.Spec.Model.Revision == "" {
		ms.Spec.Model.Revision = "main"
	}
	if ms.Spec.ServedModelName == "" {
		ms.Spec.ServedModelName = repoID(ms.Spec.Model.Source)
	}
	if ms.Spec.Accelerator.Type == "" {
		ms.Spec.Accelerator.Type = "auto"
	}
	if ms.Spec.Accelerator.Spot == nil {
		yes := true
		ms.Spec.Accelerator.Spot = &yes
	}
	if ms.Spec.Engine.GPUMemoryPercent == 0 {
		ms.Spec.Engine.GPUMemoryPercent = 90
	}
	if ms.Spec.Replicas == nil {
		one := int32(1)
		ms.Spec.Replicas = &one
	}
}

// repoID is the ORG/MODEL part of an hf://ORG/MODEL source, or "" if the source doesn't parse.
func repoID(source string) string {
	rest, ok := strings.CutPrefix(source, "hf://")
	if !ok {
		return ""
	}
	org, model, ok := strings.Cut(rest, "/")
	if !ok || org == "" || model == "" || strings.Contains(model, "/") {
		return ""
	}
	return org + "/" + model
}
