package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/deinstapel/kube-overlay-operator/controllers"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=fail,groups="",resources=pods,verbs=create,versions=v1,name=overlaynetwork.deinstapel.de,sideEffects=None,admissionReviewVersions=v1

const modulesVolumeName = "lib-modules"
const modulesPath = "/lib/modules"
const saPath = "/var/run/secrets/kubernetes.io/serviceaccount"
const sidecarApiAccess = "overlay-sidecar-api-access"

type PodInjector struct {
	Client  client.Client
	decoder *admission.Decoder
}

func (a *PodInjector) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx)
	pod := &corev1.Pod{}
	err := a.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if value, ok := pod.Labels[controllers.POD_OVERLAY_LABEL]; !ok || value != "true" {
		return admission.Allowed("no label in pod, continuing")
	}

	sa := &corev1.ServiceAccount{}
	if err = a.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: controllers.OVELRAY_NETWORK_SERVICE_ACCOUNT}, sa); err != nil {
		logger.Error(err, "Failed to get service account", "ns", req.Namespace)
		return admission.Errored(http.StatusPreconditionFailed, fmt.Errorf("sidecar service account retrieval error: %v", err))
	}
	if len(sa.Secrets) == 0 {
		logger.Error(fmt.Errorf("serviceaccount has no secrets"), "serviceaccount has no secrets", "ns", pod.Namespace)
		return admission.Errored(http.StatusPreconditionFailed, fmt.Errorf("sidecar service account does not have a token"))
	}

	if lo.ContainsBy(pod.Spec.Containers, func(c corev1.Container) bool {
		return lo.ContainsBy(c.VolumeMounts, func(v corev1.VolumeMount) bool { return v.Name == modulesVolumeName })
	}) {
		logger.Info(fmt.Sprintf("Denying pod for usage of volume %v", modulesVolumeName))
		return admission.Denied(fmt.Sprintf("volume name %s is forbidden as it is reserved by the sidecar", modulesVolumeName))
	}

	if lo.ContainsBy(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == modulesVolumeName }) {
		logger.Info(fmt.Sprintf("Denying pod for usage of volume %v", modulesVolumeName))
		return admission.Denied(fmt.Sprintf("volume name %s is forbidden as it is reserved by the sidecar", modulesVolumeName))
	}

	// Sidecar has been injected, put finalizer into pod
	pod.Finalizers = append(pod.Finalizers, controllers.OVERLAY_NETWORK_FINALIZER)

	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:            "overlaysidecar",
		Image:           os.Getenv("SIDECAR_IMAGE"), // should be equal to the local pod image
		ImagePullPolicy: corev1.PullPolicy(os.Getenv("SIDECAR_PULL_POLICY")),
		Env: []corev1.EnvVar{
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		},
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{
					corev1.Capability("SYS_MODULE"),
					corev1.Capability("NET_ADMIN"),
				},
			},
		},
		ReadinessProbe: &corev1.Probe{
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Port: intstr.FromInt(12000),
					Path: "/readyz",
				},
			},
		},
		LivenessProbe: &corev1.Probe{
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Port: intstr.FromInt(12000),
					Path: "/healthz",
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: modulesVolumeName, MountPath: modulesPath, ReadOnly: true},

			// This is quite an ugly hack to enable least-privilege operation.
			// Currently in k8s it's not possible to have a serviceaccount per container
			// xref: https://github.com/kubernetes/kubernetes/issues/66020
			// Therefore we're mounting a serviceaccount manually only into this container
			// and craft the corresponding tokens manually in order to
			// a. not interfere with existing deployments
			// b. restrict the pod to least privilege, i.e. not expose network configs to the user pod
			{Name: sidecarApiAccess, MountPath: saPath, ReadOnly: true},
		},
	})

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: modulesVolumeName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: modulesPath,
			},
		},
	}, corev1.Volume{
		Name: sidecarApiAccess,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
				{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: sa.Secrets[0].Name}, Items: []corev1.KeyToPath{
					{Key: "token", Path: "token"},
				}}},
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{
					{Key: "ca.crt", Path: "ca.crt"},
				}}},
				{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{
					{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}},
				}}},
			}},
		},
	})

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
