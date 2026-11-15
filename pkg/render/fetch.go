package render

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/podserve/podserve/pkg/platform"
)

// Names the fetch Job owns that platform leaves to the renderer: its component value and repo
// annotation. The serving ones are in render.go.
const (
	repoAnnotation = "podserve.dev/repo"
	fetchComponent = "fetch"
)

// fetchScratchSize sizes the fetch Job's scratch volume: the largest file plus the margin, rounded
// up to whole GiB, never below the minimum (docs/design/platform.md).
func fetchScratchSize(largest int64) *resource.Quantity {
	const minGib = int64(FetchVolumeMinBytes / (1 << 30))
	gib := (largest + FetchVolumeMarginBytes + (1 << 30) - 1) / (1 << 30)
	if gib < minGib {
		gib = minGib
	}
	return resource.NewQuantity(gib<<30, resource.BinarySI)
}

// FetchJob is the per-revision fetch Job: no owner reference, a TTL, the podserve-fetch service
// account, and a generic ephemeral volume sized from in.LargestFetchFileBytes.
func FetchJob(in Input) (*batchv1.Job, error) {
	hash := platform.FetchHash(in.Repo, in.Revision)
	labels := map[string]string{
		platform.ManagedByLabel: platform.ManagedByValue,
		platform.ComponentLabel: fetchComponent,
		platform.FetchHashLabel: hash,
	}
	jobLabels := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		jobLabels[k] = v
	}
	jobLabels[platform.RevisionLabel] = in.Revision

	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: batchv1.SchemeGroupVersion.String(),
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        platform.FetchJobName(in.Repo, in.Revision),
			Namespace:   platform.Namespace,
			Labels:      jobLabels,
			Annotations: map[string]string{repoAnnotation: in.Repo},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(2)),
			TTLSecondsAfterFinished: ptr(int32(platform.FetchTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: platform.FetchSA,
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "fetch",
						Image:   in.Platform.FetchImage,
						Command: []string{"python", "-I", "/app/fetch.py"},
						Env: []corev1.EnvVar{
							{Name: "PODSERVE_REPO", Value: in.Repo},
							{Name: "PODSERVE_REVISION", Value: in.Revision},
							{Name: "PODSERVE_DEST", Value: "gs://" + in.Platform.Bucket + "/" + platform.ModelPrefix(in.Repo, in.Revision)},
							{Name: "PODSERVE_HF_TOKEN_SECRET", Value: in.Platform.HFTokenSecret},
							{Name: "PODSERVE_SCRATCH", Value: "/scratch"},
							{Name: "HF_HUB_DISABLE_PROGRESS_BARS", Value: "1"},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("8Gi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}},
					}},
					Volumes: []corev1.Volume{{
						Name: "scratch",
						VolumeSource: corev1.VolumeSource{
							Ephemeral: &corev1.EphemeralVolumeSource{
								VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
									Spec: corev1.PersistentVolumeClaimSpec{
										AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
										StorageClassName: ptr(platform.StorageClassName),
										Resources: corev1.VolumeResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceStorage: *fetchScratchSize(in.LargestFetchFileBytes),
											},
										},
									},
								},
							},
						},
					}},
				},
			},
		},
	}, nil
}

// ptr is a pointer to v (optional fields the goldens pin, including 0 and false).
func ptr[T any](v T) *T { return &v }
