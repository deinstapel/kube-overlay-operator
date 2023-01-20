package coredns

import (
	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	networkv1alpha1 "github.com/deinstapel/kube-overlay-operator/api/v1alpha1"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc" // pull this in here, because we want it excluded if plugin.cfg doesn't have k8s
	"k8s.io/klog/v2"
)

const pluginName = "kovop"

var log = clog.NewWithPlugin(pluginName)
var scheme = runtime.NewScheme()

func init() {
	plugin.Register(pluginName, setup)
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(networkv1alpha1.AddToScheme(scheme))
}

func setup(c *caddy.Controller) error {
	// Do not call klog.InitFlags(nil) here.  It will cause reload to panic.
	klog.SetLogger(logr.New(&loggerAdapter{log}))

	k, err := kovopParse(c)
	if err != nil {
		return plugin.Error(pluginName, err)
	}

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		k.Next = next
		return k
	})

	return nil
}

func kovopParse(c *caddy.Controller) (*Kovop, error) {
	var (
		k   *Kovop
		err error
	)

	i := 0
	for c.Next() {
		if i > 0 {
			return nil, plugin.ErrOnce
		}
		i++

		k, err = ParseStanza(c)
		if err != nil {
			return k, err
		}
	}
	return k, nil
}

// ParseStanza parses a kovop stanza
func ParseStanza(c *caddy.Controller) (*Kovop, error) {
	k := NewKovop([]string{""})
	for c.NextBlock() {
		switch c.Val() {
		case "namespace":
			{
				args := c.RemainingArgs()
				if len(args) != 1 {
					return nil, c.ArgErr()
				}
				k.namespace = args[0]
			}
		case "cluster_domain":
			{
				args := c.RemainingArgs()
				if len(args) != 1 {
					return nil, c.ArgErr()
				}
				k.clusterDomain = args[0]
			}
		case "network_name":
			{
				args := c.RemainingArgs()
				if len(args) != 1 {
					return nil, c.ArgErr()
				}
				k.networkName = args[0]
			}
		}
	}
	return k, nil
}
