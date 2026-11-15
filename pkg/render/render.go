package render

import (
	"encoding/json"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/podserve/podserve/pkg/api/v1alpha1"
	"github.com/podserve/podserve/pkg/platform"
)

// Names podserve owns that platform leaves to the renderer: the serving component value and GKE's
// node-selector keys. The fetch Job's own are in fetch.go.
const (
	serverComponent         = "server"
	nodeSelectorAccelerator = "cloud.google.com/gke-accelerator"
	nodeSelectorGPUDriver   = "cloud.google.com/gke-gpu-driver-version"
	nodeSelectorSpot        = "cloud.google.com/gke-spot"
)

// Record is the ModelService object to server-side apply (spec, labels; no status).
func Record(in Input) (*v1alpha1.ModelService, error) {
	if in.Service == nil {
		return nil, fmt.Errorf("render: nil ModelService")
	}
	out := in.Service.DeepCopy() // never mutate the caller's object
	out.APIVersion = v1alpha1.GroupVersion.String()
	out.Kind = v1alpha1.Kind
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	out.Labels[platform.ManagedByLabel] = platform.ManagedByValue
	out.OwnerReferences = nil
	out.Status = v1alpha1.ModelServiceStatus{}
	return out, nil
}

// Serving is the Deployment, Service and PodMonitoring, each owned by owner.
func Serving(in Input, owner Owner) (*appsv1.Deployment, *corev1.Service, *unstructured.Unstructured, error) {
	if in.Service == nil {
		return nil, nil, nil, fmt.Errorf("render: nil ModelService")
	}
	name := in.Service.Name

	servingLabels := map[string]string{
		platform.ManagedByLabel: platform.ManagedByValue,
		platform.ComponentLabel: serverComponent,
		platform.ModelLabel:     name,
		platform.RevisionLabel:  in.Revision,
	}
	podLabels := make(map[string]string, len(servingLabels))
	for k, v := range servingLabels {
		podLabels[k] = v
	}
	metricLabels := make(map[string]string, len(servingLabels)-1)
	for k, v := range servingLabels {
		if k != platform.RevisionLabel {
			metricLabels[k] = v
		}
	}
	owners := []metav1.OwnerReference{owner}

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       platform.Namespace,
			Labels:          servingLabels,
			OwnerReferences: owners,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas(in.Service.Spec.Replicas),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{platform.ModelLabel: name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec:       podSpec(in),
			},
		},
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       platform.Namespace,
			Labels:          metricLabels,
			OwnerReferences: owners,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{platform.ModelLabel: name},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       platform.ServingPort,
				TargetPort: intstr.FromString("http"),
			}},
		},
	}

	pm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "monitoring.googleapis.com/v1",
		"kind":       "PodMonitoring",
		"metadata": map[string]interface{}{
			"name":            name,
			"namespace":       platform.Namespace,
			"labels":          metricLabels,
			"ownerReferences": owners,
		},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{platform.ModelLabel: name},
			},
			"endpoints": []interface{}{map[string]interface{}{
				"port":     "http",
				"path":     "/metrics",
				"interval": "15s",
			}},
		},
	}}

	// An unstructured object may hold only JSON-shaped values (map[string]any, []any, string, int64,
	// bool, ...); typed values such as map[string]string or []metav1.OwnerReference make
	// unstructured.DeepCopy panic. Round-trip through JSON to normalise.
	raw, err := json.Marshal(pm.Object)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("render: PodMonitoring: %w", err)
	}
	pm = &unstructured.Unstructured{}
	if err := pm.UnmarshalJSON(raw); err != nil {
		return nil, nil, nil, fmt.Errorf("render: PodMonitoring: %w", err)
	}
	return dep, svc, pm, nil
}

// podSpec is the serving pod: the podserve-model identity, the pinned vLLM container and its shm
// volume, on the accelerator the profile chose.
func podSpec(in Input) corev1.PodSpec {
	nodeSelector := map[string]string{
		nodeSelectorAccelerator: in.Profile.GPUType,
		nodeSelectorGPUDriver:   "latest",
	}
	if spot := in.Service.Spec.Accelerator.Spot; spot == nil || *spot {
		nodeSelector[nodeSelectorSpot] = "true"
	}

	return corev1.PodSpec{
		ServiceAccountName:            platform.ModelSA,
		EnableServiceLinks:            ptr(false),
		TerminationGracePeriodSeconds: ptr(int64(25)),
		NodeSelector:                  nodeSelector,
		Containers:                    []corev1.Container{vllmContainer(in)},
		Volumes: []corev1.Volume{{
			Name: "shm",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: resource.NewQuantity(8<<30, resource.BinarySI),
				},
			},
		}},
	}
}

// vllmContainer is the serving container: vLLM reads the model straight from GCS, sized for the
// profile's shape. Arguments podserve owns come first, in a fixed order; spec.engine.extraArgs
// follow as given.
func vllmContainer(in Input) corev1.Container {
	loaderExtra := map[string]any{"memory_limit": int64(4 << 30)}
	if in.Profile.TensorParallelSize > 1 {
		loaderExtra["distributed"] = true // only TP>1 needs the runai_streamer sharding setup
	}
	extra, err := json.Marshal(loaderExtra) // map keys sorted, no spaces
	if err != nil {
		panic("render: model-loader-extra-config: " + err.Error()) // constant map: cannot fail
	}

	args := []string{
		platform.ModelURI(in.Platform.Bucket, in.Repo, in.Revision),
		"--load-format=runai_streamer",
		"--model-loader-extra-config=" + string(extra),
		"--served-model-name=" + in.Service.Spec.ServedModelName,
		"--tensor-parallel-size=" + strconv.Itoa(in.Profile.TensorParallelSize),
		"--max-model-len=" + strconv.Itoa(in.Profile.MaxModelLen),
		fmt.Sprintf("--gpu-memory-utilization=0.%02d", in.Profile.GPUMemoryPercent),
		"--dtype=" + in.Profile.Dtype,
		"--port=" + strconv.Itoa(platform.ServingPort),
	}
	args = append(args, in.Service.Spec.Engine.ExtraArgs...)

	probe := corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")},
		},
		PeriodSeconds: 10,
	}
	startup := probe
	startup.FailureThreshold = 180

	return corev1.Container{
		Name:           "vllm",
		Image:          in.Platform.VLLMImage,
		Args:           args,
		Env:            []corev1.EnvVar{{Name: "HF_HUB_OFFLINE", Value: "1"}},
		Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: platform.ServingPort}},
		StartupProbe:   &startup,
		ReadinessProbe: &probe,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse(in.Profile.Shape.CPU),
				corev1.ResourceMemory:           resource.MustParse(in.Profile.Shape.Memory),
				corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
			},
			Limits: corev1.ResourceList{
				"nvidia.com/gpu": *resource.NewQuantity(int64(in.Profile.GPUCount), resource.BinarySI),
			},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "shm", MountPath: "/dev/shm"}},
	}
}

// replicas copies the spec's replica count, defaulting to 1 (the CRD's default) when nil.
func replicas(p *int32) *int32 {
	if p == nil {
		return ptr(int32(1))
	}
	return ptr(*p)
}
