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

package main

import (
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"golang.org/x/sys/unix"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/klauspost/compress/zstd"
	"github.com/pmorjan/kmod"
	"github.com/ulikunitz/xz"

	networkv1alpha1 "github.com/deinstapel/kube-overlay-operator/api/v1alpha1"
	"github.com/deinstapel/kube-overlay-operator/tunneler"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(networkv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":12001", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":12000", "The address the probe endpoint binds to.")
	// in sidecar mode, we want to only read the resource in the namespace

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	namespace, ok := os.LookupEnv("POD_NAMESPACE")
	if !ok {
		setupLog.Error(fmt.Errorf("POD_NAMESPACE not set"), "set POD_NAMESPACE for sidecar")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   12002,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false,
		Namespace:              namespace,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	k, err := kmod.New(
		kmod.SetInitFunc(modInitFunc),
	)
	if err != nil {
		setupLog.Error(err, "unable to initialize kmod loader")
		os.Exit(1)
	}
	if err := k.Load("fou", "", 0); err != nil {
		setupLog.Error(err, "unable to load fou")
		os.Exit(1)
	}
	if err := k.Load("ipip", "", 0); err != nil {
		setupLog.Error(err, "unable to load ipip")
		os.Exit(1)
	}

	// Pod started in sidecar mode
	if err := (&tunneler.TunnelReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OverlayTunneler")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// modInitFunc supports uncompressed files and gzip and xz compressed files
func modInitFunc(path, params string, flags int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	switch filepath.Ext(path) {
	case ".gz":
		rd, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer rd.Close()
		return initModule(rd, params)
	case ".xz":
		rd, err := xz.NewReader(f)
		if err != nil {
			return err
		}
		return initModule(rd, params)
	case ".zst":
		rd, err := zstd.NewReader(f)
		if err != nil {
			return err
		}
		defer rd.Close()
		return initModule(rd, params)
	}

	// uncompressed file, first try finitModule then initModule
	if err := finitModule(int(f.Fd()), params); err != nil {
		if err == unix.ENOSYS {
			return initModule(f, params)
		}
	}
	return nil
}

// finitModule inserts a module file via syscall finit_module(2)
func finitModule(fd int, params string) error {
	return unix.FinitModule(fd, params, 0)
}

// initModule inserts a module via syscall init_module(2)
func initModule(rd io.Reader, params string) error {
	buf, err := io.ReadAll(rd)
	if err != nil {
		return err
	}
	return unix.InitModule(buf, params)
}
