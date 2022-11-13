/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	networkv1alpha1 "github.com/deinstapel/kube-overlay-operator/api/v1alpha1"
)

const OVERLAY_NETWORK_FINALIZER = "network.deinstapel.de/network-in-use"

// OverlayNetworkReconciler reconciles a OverlayNetwork object
type OverlayNetworkReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=network.deinstapel.de,resources=overlaynetworks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=network.deinstapel.de,resources=overlaynetworks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=network.deinstapel.de,resources=overlaynetworks/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the OverlayNetwork object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *OverlayNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	nw := &networkv1alpha1.OverlayNetwork{}
	if err := r.Get(ctx, req.NamespacedName, nw); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if nw.DeletionTimestamp != nil {
		// has been deleted.
		// check if object is empty
		return r.handleDeletion(ctx, req, nw)
	} else {
		if controllerutil.AddFinalizer(nw, OVERLAY_NETWORK_FINALIZER) {
			if err := r.Update(ctx, nw); err != nil {
				if !errors.IsNotFound(err) {
					logger.Error(err, "failed setting up finalizer for overlay network", "network", nw.Name)
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OverlayNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkv1alpha1.OverlayNetwork{}).
		Complete(r)
}

// Handle pod or annotation key changes
func (r *OverlayNetworkReconciler) handleDeletion(ctx context.Context, req ctrl.Request, nw *networkv1alpha1.OverlayNetwork) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(nw, OVERLAY_NETWORK_FINALIZER) {
		return ctrl.Result{}, nil
	}

	// As long as we have allocations, we can not remove the network
	if len(nw.Status.Allocations) == 0 {

		// no allocations left, remove finalizer and push change to k8s api
		if controllerutil.RemoveFinalizer(nw, OVERLAY_NETWORK_FINALIZER) {
			if err := r.Update(ctx, nw); err != nil {
				if errors.IsNotFound(err) {
					return ctrl.Result{}, nil
				}
				// conflict, idk whatever, requeue
				return ctrl.Result{}, err
			}
		}
	}
	return ctrl.Result{}, nil
}
