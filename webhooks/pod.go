package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=fail,groups="",resources=pods,verbs=create,versions=v1,name=overlaynetwork.deinstapel.de

const modulesVolumeName = "lib-modules"
const modulesPath = "/lib/modules"

type PodInjector struct {
	Client  client.Client
	decoder *admission.Decoder
}

func (a *PodInjector) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	err := a.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if value, ok := pod.Labels["network.deinstapel.de/inject-sidecar"]; !ok || value != "true" {
		return admission.Allowed("no label in pod, continuing")
	}

	// FIXME: this opens up a potential attack vector for the pod

	if lo.ContainsBy(pod.Spec.Containers, func(c corev1.Container) bool {
		return lo.ContainsBy(c.VolumeMounts, func(v corev1.VolumeMount) bool { return v.Name == modulesVolumeName })
	}) {
		return admission.Denied(fmt.Sprintf("volume name %s is forbidden as it is reserved by the sidecar", modulesVolumeName))
	}

	if lo.ContainsBy(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == modulesVolumeName }) {
		return admission.Denied(fmt.Sprintf("volume name %s is forbidden as it is reserved by the sidecar", modulesVolumeName))
	}

	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:  "overlaysidecar",
		Image: "", // should be equal to the local pod image
		Env: []corev1.EnvVar{
			{Name: "RUN_MODE", Value: "SIDECAR"},
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		},
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{
					corev1.Capability("SYS_MODULE"),
					corev1.Capability("NET_ADMIN"),
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: modulesVolumeName, MountPath: modulesPath, ReadOnly: true},
		},
	})

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: modulesVolumeName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: modulesPath,
			},
		},
	})

	// TODO: Serviceaccount privileges?

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// podAnnotator implements admission.DecoderInjector.
// A decoder will be automatically injected.

// InjectDecoder injects the decoder.
func (a *PodInjector) InjectDecoder(d *admission.Decoder) error {
	a.decoder = d
	return nil
}
