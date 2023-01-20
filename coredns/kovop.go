package coredns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/nonwriter"
	"github.com/coredns/coredns/plugin/pkg/response"
	"github.com/coredns/coredns/request"
	networkv1alpha1 "github.com/deinstapel/kube-overlay-operator/api/v1alpha1"
	"github.com/miekg/dns"
	"github.com/samber/lo"
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

type Kovop struct {
	client.Client
	Scheme        *runtime.Scheme
	Next          plugin.Handler
	Zones         []string
	cache         map[uint32]net.IP
	namespace     string
	clusterDomain string
	networkName   string
	mgr           ctrl.Manager
}

func NewKovop(zones []string) *Kovop {
	return &Kovop{
		Zones:         zones,
		namespace:     "",
		clusterDomain: "cluster.local",
	}
}

func (k *Kovop) WatchNetwork(ctx context.Context) error {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         false,
		Namespace:              k.namespace,
		MetricsBindAddress:     "0",
		HealthProbeBindAddress: "0",
		BaseContext:            func() context.Context { return ctx },
	})

	if err != nil {
		return err
	}
	k.mgr = mgr

	return ctrl.NewControllerManagedBy(mgr).For(&networkv1alpha1.OverlayNetwork{}).Complete(k)
}

func (k *Kovop) Name() string {
	return pluginName
}

func (r *Kovop) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	nw := &networkv1alpha1.OverlayNetwork{}
	if err := r.Get(ctx, req.NamespacedName, nw); err != nil {
		if k8serrors.IsNotFound(err) {
			r.cache = make(map[uint32]net.IP)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	newCache := make(map[uint32]net.IP)
	for _, al := range nw.Status.Allocations {
		podIp := net.ParseIP(al.PodIP)
		tunnelIp := net.ParseIP(al.IP)
		if podIp != nil && tunnelIp != nil {
			newCache[ip2int(podIp)] = tunnelIp
		}
	}
	r.cache = newCache

	return ctrl.Result{}, nil
}

func (k *Kovop) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	nw := nonwriter.New(w)
	state := request.Request{W: nw, Req: r}
	qname := state.QName()
	zone := plugin.Zones(k.Zones).Matches(qname)
	if zone == "" {
		// We won't return any other than our locally registered zone
		return dns.RcodeServerFailure, errors.New("invalid dns query")
	}

	subdomain := strings.TrimSuffix(qname, zone)
	r.Question[0].Name = fmt.Sprintf("%v.%v.svc.%v.", subdomain, k.namespace, k.clusterDomain)

	// Forward request to k8s plugin
	rcode, err := plugin.NextOrFailure(k.Name(), k.Next, ctx, nw, r)
	if err != nil {
		return rcode, err
	}

	ty, _ := response.Typify(nw.Msg, time.Now().UTC())
	cl := response.Classify(ty)

	// if response is Denial or Error pass through also if the type is Delegation pass through
	if cl == response.Denial || cl == response.Error || ty == response.Delegation {
		w.WriteMsg(nw.Msg)
		return 0, nil
	}
	if ty != response.NoError {
		w.WriteMsg(nw.Msg)
		return 0, plugin.Error("minimal", fmt.Errorf("unhandled response type %q for %q", ty, nw.Msg.Question[0].Name))
	}

	resp := lo.Filter(lo.Map(nw.Msg.Answer, func(d dns.RR, _ int) dns.RR {
		if a, ok := d.(*dns.A); ok {

			if mapped, ok := k.cache[ip2int(a.A)]; ok {
				return &dns.A{Hdr: a.Hdr, A: mapped}
			}
		}
		return nil
	}), func(d dns.RR, _ int) bool {
		return d != nil
	})

	if len(resp) == 0 {
		// No mapped IPs available ~> return NXDOMAIN
		resp := &dns.Msg{
			MsgHdr:   nw.Msg.MsgHdr,
			Compress: nw.Msg.Compress,
		}
		resp.MsgHdr.Rcode = dns.RcodeNameError
		w.WriteMsg(resp)
		return 0, nil
	}

	d := &dns.Msg{
		MsgHdr:   nw.Msg.MsgHdr,
		Compress: nw.Msg.Compress,
		Question: nw.Msg.Question,
		Answer:   resp,
		Ns:       nil,
		Extra:    nil,
	}
	w.WriteMsg(d)
	return 0, nil
}
