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
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

const OVELRAY_NETWORK_SERVICE_ACCOUNT = "overlay-network-sidecar"

// OverlayNetworkReconciler reconciles a OverlayNetwork object
type NamespaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create
//+kubebuilder:rbac:groups="authorization.k8s.io",resources=rolebindings,verbs=get;list;watch;create

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the OverlayNetwork object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Name, Name: OVELRAY_NETWORK_SERVICE_ACCOUNT}, sa); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := r.reconcileServiceAccount(ctx, req.Name); err != nil {
			if !errors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
		}
	}

	if len(sa.Secrets) == 0 {
		// We're running on k8s >- 1.24, so no secrets are created automatically.

		if err := r.reconcileSecret(ctx, sa); err != nil {
			if !errors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
		}
	}

	role := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Name, Name: OVELRAY_NETWORK_SERVICE_ACCOUNT}, role); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := r.reconcileRole(ctx, req.Name); err != nil {
			if !errors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
		}
	}

	logger.Info("reconciled namespace", "ns", req.Name)

	return ctrl.Result{}, nil
}

func (r *NamespaceReconciler) reconcileServiceAccount(ctx context.Context, ns string) error {
	sa := &corev1.ServiceAccount{}
	sa.Namespace = ns
	sa.Name = OVELRAY_NETWORK_SERVICE_ACCOUNT
	return r.Client.Create(ctx, sa)
}

func (r *NamespaceReconciler) reconcileSecret(ctx context.Context, sa *corev1.ServiceAccount) error {
	secret := &corev1.Secret{}
	secretName := fmt.Sprintf("%s-token", sa.Name)
	if err := r.Get(ctx, client.ObjectKey{Namespace: sa.Namespace, Name: secretName}, secret); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		secret.Namespace = sa.Namespace
		secret.Name = secretName
		secret.Type = "kubernetes.io/service-account-token"
		secret.Annotations = map[string]string{
			"kubernetes.io/service-account.name": sa.Name,
		}
		if err := r.Client.Create(ctx, secret); err != nil {
			if !errors.IsAlreadyExists(err) {
				return err
			}
		}
	}
	return nil
}

func (r *NamespaceReconciler) reconcileRole(ctx context.Context, ns string) error {
	role := &rbacv1.RoleBinding{}
	role.Namespace = ns
	role.Name = OVELRAY_NETWORK_SERVICE_ACCOUNT
	role.Subjects = []rbacv1.Subject{
		{Kind: "ServiceAccount", APIGroup: "", Namespace: ns, Name: OVELRAY_NETWORK_SERVICE_ACCOUNT},
	}
	role.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "ClusterRole",
		Name:     OVELRAY_NETWORK_SERVICE_ACCOUNT,
	}
	return r.Client.Create(ctx, role)
}

// SetupWithManager sets up the controller with the Manager.
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Complete(r)
}
