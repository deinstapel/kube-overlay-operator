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

package tunneler

import (
	"context"
	goerrors "errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strings"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nwApi "github.com/deinstapel/kube-overlay-operator/api/v1alpha1"
	"github.com/itchyny/base58-go"
	"github.com/samber/lo"
	"github.com/vishvananda/netlink"
)

// TunnelReconciler reconciles a OverlayNetwork object and creates the ipip tunnels
type TunnelReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	myPodName     string
	networkToName map[string]int32
	networkIndex  atomic.Int32
}

type LinkInfo struct {
	RemoteIP         string
	LocalIP          string
	InTunnelLocalIP  string
	InTunnelRemoteIP string
	Processed        bool
}

type routeState struct {
	net    *net.IPNet
	router net.IP
}

//+kubebuilder:rbac:groups=network.deinstapel.de,resources=overlaynetworks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=network.deinstapel.de,resources=overlaynetworks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=network.deinstapel.de,resources=overlaynetworks/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *TunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	nw := &nwApi.OverlayNetwork{}
	if err := r.Get(ctx, req.NamespacedName, nw); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	localPodRouter, isRouter := lo.Find(nw.Status.Routers, func(t nwApi.OverlayNetworkIPAllocation) bool { return t.PodName == r.myPodName })

	targetLinkState := make(map[string]*LinkInfo)

	if isRouter {
		otherPods := lo.Filter(nw.Status.Allocations, func(item nwApi.OverlayNetworkIPAllocation, i int) bool { return item.PodName != r.myPodName })
		targetLinkState = lo.SliceToMap(otherPods, func(item nwApi.OverlayNetworkIPAllocation) (string, *LinkInfo) {
			linkName := r.getTunnelId(nw, &item)
			return linkName, &LinkInfo{
				RemoteIP:         item.PodIP,
				LocalIP:          localPodRouter.PodIP,
				InTunnelLocalIP:  localPodRouter.IP,
				InTunnelRemoteIP: item.IP,
				Processed:        false,
			}
		})
	} else {
		localPodMember, isMember := lo.Find(nw.Status.Allocations, func(t nwApi.OverlayNetworkIPAllocation) bool { return t.PodName == r.myPodName })
		if isMember {
			// I am no router but a client
			targetLinkState = lo.SliceToMap(nw.Status.Routers, func(item nwApi.OverlayNetworkIPAllocation) (string, *LinkInfo) {
				linkName := r.getTunnelId(nw, &item)
				return linkName, &LinkInfo{
					RemoteIP:         item.PodIP,
					LocalIP:          localPodMember.PodIP,
					InTunnelLocalIP:  localPodMember.IP,
					InTunnelRemoteIP: item.IP,
					Processed:        false,
				}
			})
		}
	}

	logger.Info("Reconciled network space", "ls", targetLinkState)
	err := r.reconcileTunnelInterfaces(nw, targetLinkState, isRouter)
	return ctrl.Result{}, err
}

func (r *TunnelReconciler) getNetworkId(nw *nwApi.OverlayNetwork) rune {
	var nwId int32
	if readNw, ok := r.networkToName[nw.Name]; !ok {
		nwId = r.networkIndex.Add(1) - 1
		r.networkToName[nw.Name] = nwId
	} else {
		nwId = readNw
	}
	return 'A' + nwId
}

func (r *TunnelReconciler) getTunnelId(nw *nwApi.OverlayNetwork, pod *nwApi.OverlayNetworkIPAllocation) string {
	networkIndex := r.getNetworkId(nw)
	xh := fnv.New64()
	xh.Write([]byte(pod.PodName))
	encoded := string(base58.FlickrEncoding.EncodeUint64(xh.Sum64()))
	return fmt.Sprintf("ov-%c-%s", networkIndex, encoded)
}

// SetupWithManager sets up the controller with the Manager.
func (r *TunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.networkIndex.Store(0)
	r.networkToName = make(map[string]int32)
	if pn, ok := os.LookupEnv("POD_NAME"); !ok {
		return goerrors.New("please set POD_NAME in client mode")
	} else {
		r.myPodName = pn
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nwApi.OverlayNetwork{}).
		Complete(r)
}

func (r *TunnelReconciler) reconcileTunnelInterfaces(nw *nwApi.OverlayNetwork, targetState map[string]*LinkInfo, isRouter bool) error {

	// Ensure the receiving side is setup properly
	if err := r.reconcileFOU(nw, len(targetState) > 0); err != nil {
		return err
	}

	// Get all network links
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	// Check the links we currently have, if there are some that need change or deletion
	localNet := r.getNetworkId(nw)
	prefix := fmt.Sprintf("ov-%c-", localNet)
	for i := range links {

		// Get link attributes
		link := links[i]
		attrs := link.Attrs()

		// If the link doesn't start with the Overlay Prefix, skip it
		if !strings.HasPrefix(attrs.Name, prefix) {
			// fmt.Printf("link %v does not belong to network %v, skipping\n", attrs.Name, nw.Name)
			continue
		}

		if linkInfo, ok := targetState[attrs.Name]; ok {
			// Link exists in target state, reconcile link.
			// FIXME: add reconciliation for this, e.g. if the endpoint changed, might occur if a pod with the same name restarts and gets a different Pod IP
			linkInfo.Processed = true
		} else {
			// Link does not exist in target state, remove from state
			// This also removes all routes populated for the link
			if err := netlink.LinkDel(link); err != nil {
				return err
			}
		}
	}

	for linkName, linkInfo := range targetState {
		if linkInfo.Processed {
			continue
		}
		// This link does not yet exist, so set it up.
		if err := r.setupTunnelIface(nw, linkName, linkInfo, isRouter); err != nil {
			return err
		}
	}

	// at this point all links exist, and the allocatable-cidr-routes are setup properly.
	if !isRouter {
		return r.reconcileRoutesMember(nw, targetState)
	}

	return nil
}

func (r *TunnelReconciler) setupTunnelIface(nw *nwApi.OverlayNetwork, linkName string, linkInfo *LinkInfo, isRouter bool) error {
	// This link does not yet exist, so set it up.
	if err := netlink.LinkAdd(&netlink.Iptun{
		EncapType:  1, // TUNNEL_ENCAP_FOU from https://github.com/torvalds/linux/blob/master/include/uapi/linux/if_tunnel.h
		EncapSport: uint16(nw.Spec.Port),
		EncapDport: uint16(nw.Spec.Port),
		EncapFlags: 0,
		Local:      net.ParseIP(linkInfo.LocalIP),
		Remote:     net.ParseIP(linkInfo.RemoteIP),
		Ttl:        200,
		LinkAttrs: netlink.LinkAttrs{
			Name: linkName,
		},
	}); err != nil {
		return err
	}

	link, err := netlink.LinkByName(linkName)
	if err != nil {
		return err
	}

	// Set the link state
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}

	// Add IP
	_, mask, err := net.ParseCIDR(nw.Spec.AllocatableCIDR)
	if err != nil {
		return err
	}

	// add the whole /allocatable as route by default, except for when we are a router, then we only want the
	// /32 ip
	if isRouter {
		mask.Mask = net.CIDRMask(32, 32)
	}
	if err := netlink.AddrAdd(link, &netlink.Addr{
		IPNet: &net.IPNet{IP: net.ParseIP(linkInfo.InTunnelLocalIP), Mask: mask.Mask},
	}); err != nil {
		return err
	}

	if isRouter {
		// We are a router pod, so add the route to the pods overlay IP into our local table
		if err := netlink.RouteAdd(&netlink.Route{
			LinkIndex: link.Attrs().Index,
			Scope:     netlink.SCOPE_LINK,
			Dst:       &net.IPNet{IP: net.ParseIP(linkInfo.InTunnelRemoteIP), Mask: mask.Mask},
			Src:       net.ParseIP(linkInfo.InTunnelLocalIP),
		}); err != nil {
			return err
		}
	}
	return nil
}

// reconcileFOU ensures that the receiving side of the FOU stack is setup properly
// we consider the FOU required if there is one or more links present for the given network (means the pod is part of it)
func (r *TunnelReconciler) reconcileFOU(nw *nwApi.OverlayNetwork, shouldBePresent bool) error {
	fouList, err := netlink.FouList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	fouLink, fouIsPresent := lo.Find(fouList, func(f netlink.Fou) bool { return f.Port == nw.Spec.Port })
	if fouIsPresent && !shouldBePresent {
		return netlink.FouDel(fouLink)
	}
	if shouldBePresent && !fouIsPresent {
		return netlink.FouAdd(netlink.Fou{
			Family:    netlink.FAMILY_V4,
			Port:      nw.Spec.Port,
			Protocol:  4, // IPIP
			EncapType: netlink.FOU_ENCAP_DIRECT,
		})
	}
	return nil
}

// reconcileRoutesMember matches the currently deployed route for interfaces in a single overlay network to match extraCidrs
func (r *TunnelReconciler) reconcileRoutesMember(nw *nwApi.OverlayNetwork, targetState map[string]*LinkInfo) error {
	for linkName, linkInfo := range targetState {
		link, err := netlink.LinkByName(linkName)
		if err != nil {
			return err
		}

		routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		allowedCidrs := lo.FilterMap(append(nw.Spec.RoutableCIDRs, nw.Spec.AllocatableCIDR), func(e string, i int) (*net.IPNet, bool) {
			_, net, err := net.ParseCIDR(e)
			if err != nil {
				return nil, false
			}
			return net, true
		})
		processedCidrs := []routeState{}

		for i := range routes {
			route := &routes[i]
			cidr, isAllowed := lo.Find(allowedCidrs, func(i *net.IPNet) bool {
				gwValid := lo.ContainsBy(nw.Status.Routers, func(r nwApi.OverlayNetworkIPAllocation) bool { return net.ParseIP(r.IP).Equal(route.Gw) })
				return netEqual(route.Dst, i) && gwValid
			})
			if !isAllowed {
				// route should not be here
				if err := netlink.RouteDel(route); err != nil {
					return err
				}
			} else {
				// Push the route into our own intermediate state, with the target net and the gateway
				processedCidrs = append(processedCidrs, routeState{net: cidr, router: route.Gw})
			}
		}

		for _, route := range allowedCidrs {
			for _, router := range nw.Status.Routers {
				// Check for duplicates
				if lo.ContainsBy(processedCidrs, func(p routeState) bool { return netEqual(p.net, route) && net.ParseIP(router.IP).Equal(p.router) }) {
					continue
				}

				// Add route
				if err := netlink.RouteAdd(&netlink.Route{
					LinkIndex: link.Attrs().Index,
					Scope:     netlink.SCOPE_LINK,
					Dst:       route,
					Src:       net.ParseIP(linkInfo.InTunnelLocalIP),
					Gw:        net.ParseIP(router.IP),
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func netEqual(n1 *net.IPNet, n2 *net.IPNet) bool {
	n1ones, n1bits := n1.Mask.Size()
	n2ones, n2bits := n2.Mask.Size()
	return n1ones == n2ones && n1bits == n2bits && n1.IP.Equal(n1.IP)
}
