package resolver

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"

	networkv1alpha1 "github.com/deinstapel/kube-overlay-operator/api/v1alpha1"
	"github.com/miekg/dns"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ip2int(ip net.IP) uint32 {
	if len(ip) == 16 {
		panic("no sane way to convert ipv6 into uint32")
	}
	return binary.BigEndian.Uint32(ip)
}

/**
k8s-service.network-name.$domain
*/

type NetworkCache struct {
	services map[string]bool
	ipCache  map[uint32]net.IP
}

type DNSReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	cache         map[string]NetworkCache
	namespace     string
	zone          string
	clusterDomain string
}

func NewDNSReconciler(c client.Client, scheme *runtime.Scheme, namespace string, zone string, clusterDomain string) *DNSReconciler {
	if clusterDomain == "" {
		clusterDomain = "cluster.local"
	}
	return &DNSReconciler{
		Client:        c,
		Scheme:        scheme,
		namespace:     namespace,
		zone:          zone,
		cache:         make(map[string]NetworkCache),
		clusterDomain: clusterDomain,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *DNSReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkv1alpha1.OverlayNetwork{}).
		Complete(r)
}

func (r *DNSReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	nw := &networkv1alpha1.OverlayNetwork{}
	if err := r.Get(ctx, req.NamespacedName, nw); err != nil {
		if k8serrors.IsNotFound(err) {
			delete(r.cache, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	nc := NetworkCache{
		services: make(map[string]bool),
		ipCache:  make(map[uint32]net.IP),
	}
	for _, al := range nw.Status.Allocations {
		podIp := net.ParseIP(al.PodIP).To4()
		tunnelIp := net.ParseIP(al.IP).To4()
		if podIp != nil && tunnelIp != nil {
			nc.ipCache[ip2int(podIp)] = tunnelIp
		}
	}
	for _, svc := range nw.Spec.ServiceNames {
		nc.services[svc] = true
	}
	r.cache[nw.Name] = nc

	return ctrl.Result{}, nil
}

func (r *DNSReconciler) processDnsRequest(name string, reply *dns.Msg, q *dns.Msg) {
	parts := strings.Split(strings.TrimSuffix(name, "."+r.zone), ".")
	if len(parts) != 2 {
		reply.SetRcode(q, dns.RcodeNameError)
		return
	}
	svcName := parts[0]
	nwName := parts[1]
	nw, ok := r.cache[nwName]
	if !ok {
		reply.SetRcode(q, dns.RcodeNameError)
		return
	}
	if svcOk, ok := nw.services[svcName]; !ok || !svcOk {
		reply.SetRcode(q, dns.RcodeNameError)
		return
	}

	res, err := net.LookupIP(fmt.Sprintf("%v.%v.svc.%v", svcName, r.namespace, r.clusterDomain))
	if err != nil {
		reply.SetRcode(q, dns.RcodeServerFailure)
		return
	}
	for _, ip := range res {
		v4 := ip.To4()
		if v4 == nil {
			continue
		}
		if target, ok := nw.ipCache[ip2int(v4)]; ok {
			rr, err := dns.NewRR(fmt.Sprintf("%s A %s", name, target.String()))
			if err != nil {
				continue
			}
			reply.Answer = append(reply.Answer, rr)
		}
	}

	if len(reply.Answer) > 0 {
		reply.SetRcode(q, dns.RcodeSuccess)
	} else {
		reply.SetRcode(q, dns.RcodeNameError)
	}
}

func (r *DNSReconciler) handleDnsRequest(w dns.ResponseWriter, q *dns.Msg) {
	reply := new(dns.Msg)
	reply.SetReply(q)
	reply.Compress = false

	if q.Opcode != dns.OpcodeQuery || len(reply.Question) != 1 {
		reply.SetRcode(q, dns.RcodeNotImplemented)
		w.WriteMsg(reply)
		return
	}
	q0 := reply.Question[0]
	switch q0.Qtype {
	case dns.TypeA:
		r.processDnsRequest(q0.Name, reply, q)
	default:
		reply.SetRcode(q, dns.RcodeNameError)
	}
	w.WriteMsg(reply)
}

func (r *DNSReconciler) ServeDNS(ctx context.Context) error {
	dns.HandleFunc(r.zone, r.handleDnsRequest)
	port := 5353
	server := &dns.Server{Addr: fmt.Sprintf(":%d", port), Net: "udp"}
	if err := server.ListenAndServe(); err != nil {
		return err
	}

	<-ctx.Done()
	return server.Shutdown()
}
