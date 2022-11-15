package webhooks

import (
	"context"
	"fmt"
	"net"
	"net/http"

	nwApi "github.com/deinstapel/kube-overlay-operator/api/v1alpha1"
	"github.com/samber/lo"
	v1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-v1alpha1-overlaynetwork,mutating=false,failurePolicy=fail,groups=network.deinstapel.de,resources=overlaynetworks,verbs=create;update,versions=v1alpha1,name=overlaynetwork.deinstapel.de,sideEffects=None,admissionReviewVersions=v1

// OverlayNetworkValidator performs validation on the overlay networks, e.g. mandating a valid port and valid CIDRs.
type OverlayNetworkValidator struct {
	Client  client.Client
	decoder *admission.Decoder
}

func (a *OverlayNetworkValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	nw := &nwApi.OverlayNetwork{}
	err := a.decoder.Decode(req, nw)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if req.Operation == v1.Create {
		if nw.Spec.Port <= 0 || nw.Spec.Port > 65535 {
			return admission.Denied(".spec.port must be between 1 and 65535")
		}
		if _, _, err := net.ParseCIDR(nw.Spec.AllocatableCIDR); err != nil {
			return admission.Denied(fmt.Sprintf(".spec.allocatableCIDR %v is invalid, must be in CIDR notation", nw.Spec.AllocatableCIDR))
		}
	}
	if req.Operation == v1.Update {
		nwOld := &nwApi.OverlayNetwork{}
		if err := a.decoder.DecodeRaw(req.OldObject, nwOld); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if nwOld.Spec.Port != nw.Spec.Port {
			return admission.Denied(".spec.port is immutable")
		}
		if nwOld.Spec.AllocatableCIDR != nw.Spec.AllocatableCIDR {
			return admission.Denied(".spec.allocatableCIDR is immutable")
		}
	}

	invalid, invalidIndex, hasInvalid := lo.FindIndexOf(nw.Spec.RoutableCIDRs, func(s string) bool {
		_, _, err := net.ParseCIDR(s)
		return err != nil
	})
	if hasInvalid {
		return admission.Denied(fmt.Sprintf(".spec.routableCIDRs[%d]: %v is invalid, must be in CIDR notation", invalidIndex, invalid))
	}

	return admission.Allowed("ok")
}

// InjectDecoder injects the decoder.
func (a *OverlayNetworkValidator) InjectDecoder(d *admission.Decoder) error {
	a.decoder = d
	return nil
}
