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
	"net"
	"strings"
	"time"

	goerrors "errors"

	nwApi "github.com/deinstapel/kube-overlay-operator/api/v1alpha1"
	"github.com/deinstapel/kube-overlay-operator/controllers/iputil"
	lo "github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const POD_NETWORK_MEMBER_ANNOTATION = "network.deinstapel.de/member-of"
const POD_NETWORK_ROUTER_ANNOTATION = "network.deinstapel.de/router-for"

// Format is overlay/routeName
const POD_NETWORK_EXTRA_ROUTER_ANNOTATION = "network.deinstapel.de/extra-router"
const POD_NETWORK_EXTRA_ROUTE_ANNOTATION = "network.deinstapel.de/extra-routes"

// Format is overlay/ip
const POD_IP_ANNOTATION = "network.deinstapel.de/request-ip"

const POD_FINALIZER = "network.deinstapel.de/ipam"
const POD_OVERLAY_LABEL = "network.deinstapel.de/inject-sidecar"

// PodReconciler reconciles a Pod object that is part of an OverlayNetwork
type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups="",resources=pods/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Could not retrieve pod")
		return ctrl.Result{}, err
	}

	if v, ok := pod.Labels[POD_OVERLAY_LABEL]; !ok || v != "true" {
		// skip reconcilation early
		return ctrl.Result{}, nil
	}

	memberNetworksList := lo.Filter(strings.Split(pod.Annotations[POD_NETWORK_MEMBER_ANNOTATION], ","), func(s string, i int) bool { return s != "" })
	routerNetworksList := lo.Filter(strings.Split(pod.Annotations[POD_NETWORK_ROUTER_ANNOTATION], ","), func(s string, i int) bool { return s != "" })

	extraRoutersList := lo.Filter(strings.Split(pod.Annotations[POD_NETWORK_EXTRA_ROUTER_ANNOTATION], ","), func(s string, i int) bool { return s != "" })
	extraRoutesList := lo.Filter(strings.Split(pod.Annotations[POD_NETWORK_EXTRA_ROUTE_ANNOTATION], ","), func(s string, i int) bool { return s != "" })

	allNetworkList := lo.Uniq(append(memberNetworksList, routerNetworksList...))

	uniqueNetworks := lo.SliceToMap(lo.Uniq(allNetworkList), func(nw string) (string, bool) { return nw, false })
	routerNetworks := lo.SliceToMap(lo.Uniq(routerNetworksList), func(nw string) (string, bool) { return nw, false })

	// TODO: Caching? dunno
	nwList := &nwApi.OverlayNetworkList{}
	if err := r.List(ctx, nwList, client.InNamespace(pod.Namespace)); err != nil {
		logger.Error(err, "failed to list networks")
		return ctrl.Result{}, err
	}

	shouldRemoveFinalizer := false
	hasErrors := false

	if pod.DeletionTimestamp != nil {

		logger.Info(fmt.Sprintf("pod %v was deleted, removing from networks", pod.Name))
		uniqueNetworks = make(map[string]bool)
		routerNetworks = make(map[string]bool)
		shouldRemoveFinalizer = true
	}

	for i := range nwList.Items {
		nw := &nwList.Items[i]

		// Null safety!
		if nw.Status.OptionalRouters == nil {
			nw.Status.OptionalRouters = make(map[string][]nwApi.OverlayNetworkIPAllocation)
		}

		_, isRouter := routerNetworks[nw.Name]
		if _, ok := uniqueNetworks[nw.Name]; ok {

			extraRoutersPrefix := fmt.Sprintf("%s/", nw.Name)
			extraRoutersStrings := lo.Filter(extraRoutersList, func(s string, _ int) bool { return strings.HasPrefix(s, extraRoutersPrefix) })
			extraRoutesStrings := lo.Filter(extraRoutesList, func(s string, _ int) bool { return strings.HasPrefix(s, extraRoutersPrefix) })
			extraRoutersNames := lo.Map(extraRoutersStrings, func(s string, _ int) string { return strings.TrimPrefix(s, extraRoutersPrefix) })
			extraRoutesNames := lo.Map(extraRoutesStrings, func(s string, _ int) string { return strings.TrimPrefix(s, extraRoutersPrefix) })

			if err := r.allocateIP(ctx, nw, pod, isRouter, extraRoutesNames, extraRoutersNames); err != nil {
				logger.Error(err, "error allocating ip", "network", nw.Name)
				hasErrors = true
			} else {
				uniqueNetworks[nw.Name] = true
			}
		} else {
			if err := r.deallocateIP(ctx, nw, pod, isRouter); err != nil {
				logger.Error(err, "error deallocating ip", "network", nw.Name)
				hasErrors = true
			}
		}
	}

	if shouldRemoveFinalizer && !hasErrors {
		if controllerutil.RemoveFinalizer(pod, POD_FINALIZER) {
			err := r.Update(ctx, pod)
			if err != nil {
				logger.Error(err, "could not remove finalizer", "pod", pod.Name)
				hasErrors = true
			}
		}
	}

	// check if any of the uniqueNetworks didn't get processed, e.g. because the network should be there but wasn't
	anyNetworkUnprocessed := lo.Contains(lo.MapToSlice(uniqueNetworks, func(_ string, v bool) bool { return v }), false)

	// If any network of the pod had errors, reconcile later
	if hasErrors || anyNetworkUnprocessed {
		logger.Info("requeuing pod, had errors or unprocessed networks", "pod", pod.Name)
		return ctrl.Result{RequeueAfter: 1 * time.Minute, Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}

func (r *PodReconciler) allocateIP(ctx context.Context, nw *nwApi.OverlayNetwork, pod *corev1.Pod, isRouter bool, extraNetworks []string, extraRouters []string) error {
	if pod.Status.PodIP == "" {
		return goerrors.New("refusing assigning overlay IP to pod without primary IP")
	}
	logger := log.FromContext(ctx)
	var allocation nwApi.OverlayNetworkIPAllocation
	if allocFromList, allocIndex, found := lo.FindIndexOf(nw.Status.Allocations, func(item nwApi.OverlayNetworkIPAllocation) bool {
		return pod.Name == item.PodName
	}); !found {
		// The pod has no valid IP allocation in this network, so create one

		// Don't allocate new IPs to deleting network
		if nw.DeletionTimestamp != nil {
			err := goerrors.New("refusing to allocate new IP to deleting network")
			logger.Error(err, "network deleting", "network", nw.Name)
			return err
		}

		// Build a map of allocations for faster checks
		allocationMap := make(map[string]bool)
		for _, alloc := range nw.Status.Allocations {
			allocationMap[alloc.IP] = true
		}

		var requestedIp net.IP = nil
		if requestIp, ok := pod.ObjectMeta.Annotations[POD_IP_ANNOTATION]; ok {
			nwPrefix := fmt.Sprintf("%s/", nw.Name)
			requests := lo.Filter(strings.Split(requestIp, ","), func(i string, _ int) bool {
				return strings.HasPrefix(i, nwPrefix)
			})
			if len(requests) > 0 {
				ipString := strings.TrimPrefix(requests[0], nwPrefix)
				requestedIp = net.ParseIP(ipString)
			}
		}
		_, cidr, err := net.ParseCIDR(nw.Spec.AllocatableCIDR)
		if err != nil {
			logger.Error(err, "invalid CIDR for overlay network", "network", nw.Name)
			return err
		}
		if requestedIp != nil && !cidr.Contains(requestedIp) {
			err := fmt.Errorf("pod requested IP outside of allocatable CIDRs")
			logger.Error(err, "invalid allocations", "network", nw.Name, "pod", pod.Name)
			return err
		}

		// Retrieve IP address, handle pool exhausted errors
		ipAlloc, err := iputil.FirstFreeHost(nw.Spec.AllocatableCIDR, allocationMap, requestedIp)
		if err == iputil.ErrDuplicateIPRequest {
			logger.Error(err, "duplicate IP request", "network", nw.Name, "pod", pod.Name)
			return err
		} else if err == iputil.ErrAddressPoolExhausted {
			logger.Error(err, "network address pool exhaused", "network", nw.Name)
			return err
		} else if err != nil {
			logger.Error(err, "invalid CIDR for overlay network", "network", nw.Name)
			return err
		}

		allocation = nwApi.OverlayNetworkIPAllocation{
			PodName:       pod.Name,
			IP:            ipAlloc,
			PodIP:         pod.Status.PodIP,
			ExtraNetworks: extraNetworks,
		}
		// Update the allocations in the Network.Status resource
		nw.Status.Allocations = append(nw.Status.Allocations, allocation)
	} else {
		// pod has existing allocation, reuse for router processing
		allocation = allocFromList
		allocation.ExtraNetworks = extraNetworks
		nw.Status.Allocations[allocIndex] = allocation
	}

	// Record this pod in the list of optional routers
	for _, extraRouter := range extraRouters {
		if !lo.ContainsBy(nw.Status.OptionalRouters[extraRouter], func(a nwApi.OverlayNetworkIPAllocation) bool {
			return a.PodName == allocation.PodName
		}) {
			nw.Status.OptionalRouters[extraRouter] = append(nw.Status.OptionalRouters[extraRouter], allocation)
		}
	}
	// Remove this pod from all other optional router networks
	for rtName := range nw.Status.OptionalRouters {
		if lo.Contains(extraRouters, rtName) {
			continue
		}
		nw.Status.OptionalRouters[rtName] = lo.Filter(nw.Status.OptionalRouters[rtName], func(n nwApi.OverlayNetworkIPAllocation, _ int) bool { return n.PodName != pod.Name })
	}

	// Check if the pod we're processing acts as a router, if so also populate network.Routers
	if isRouter {
		// Pod is a router, ensure it's in the list
		if !lo.ContainsBy(nw.Status.Routers, func(n nwApi.OverlayNetworkIPAllocation) bool { return n.PodName == pod.Name }) {
			nw.Status.Routers = append(nw.Status.Routers, allocation)
		}
	} else {
		// Pod is no router, so remove from list
		nw.Status.Routers = lo.Filter(nw.Status.Routers, func(n nwApi.OverlayNetworkIPAllocation, i int) bool { return n.PodName != pod.Name })
	}

	// Only update the network if it got changed
	if err := r.Status().Update(ctx, nw); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "could not update network status", "network", nw.Name)
			return err
		}
	}

	return nil
}

// handlePodDeletion manages pod or annotation key deletions
func (r *PodReconciler) deallocateIP(ctx context.Context, nw *nwApi.OverlayNetwork, pod *corev1.Pod, isRouter bool) error {
	logger := log.FromContext(ctx)

	nw.Status.Allocations = lo.Filter(nw.Status.Allocations, func(item nwApi.OverlayNetworkIPAllocation, index int) bool {
		return item.PodName != pod.Name
	})
	nw.Status.Routers = lo.Filter(nw.Status.Routers, func(item nwApi.OverlayNetworkIPAllocation, index int) bool {
		return item.PodName != pod.Name
	})
	// Remove this pod from optional router networks
	for rtName := range nw.Status.OptionalRouters {
		nw.Status.OptionalRouters[rtName] = lo.Filter(nw.Status.OptionalRouters[rtName], func(n nwApi.OverlayNetworkIPAllocation, _ int) bool {
			return n.PodName != pod.Name
		})
	}

	if err := r.Status().Update(ctx, nw); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "could not update network status", "network", nw.Name)
			return err
		}
	}
	return nil
}
