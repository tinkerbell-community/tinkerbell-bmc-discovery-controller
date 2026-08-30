// The tinkerbell-bmc-discovery-controller manager discovers BMCs over mDNS
// and maintains Tinkerbell Machine and Hardware resources from their
// Redfish inventory.
package main

import (
	"flag"
	"os"
	"strings"
	"time"

	bmcv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/controller"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/inventory"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
	syncpkg "github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/sync"
)

func main() {
	var (
		namespace         string
		serviceTypes      string
		mdnsDomain        string
		browseInterval    time.Duration
		browseWindow      time.Duration
		resyncInterval    time.Duration
		collectTimeout    time.Duration
		credentialsSecret string
		redfishPort       int
		insecureTLS       bool
		leaderElect       bool
		metricsAddr       string
		probeAddr         string
	)
	flag.StringVar(&namespace, "namespace", "tink", "Namespace for created resources and the credentials secret.")
	flag.StringVar(&serviceTypes, "service-types", "_redfish._tcp,_obmc_redfish._tcp", "Comma-separated DNS-SD service types to browse.")
	flag.StringVar(&mdnsDomain, "mdns-domain", "local.", "mDNS browse domain.")
	flag.DurationVar(&browseInterval, "browse-interval", 5*time.Minute, "Time between mDNS browse cycles.")
	flag.DurationVar(&browseWindow, "browse-window", 30*time.Second, "Duration of each mDNS browse cycle.")
	flag.DurationVar(&resyncInterval, "resync-interval", time.Hour, "Inventory refresh interval for known BMCs.")
	flag.DurationVar(&collectTimeout, "collect-timeout", 2*time.Minute, "Timeout for one inventory collection.")
	flag.StringVar(&credentialsSecret, "credentials-secret", "bmc-discovery-credentials", "Name of the Secret holding BMC username/password keys.")
	flag.IntVar(&redfishPort, "redfish-port", 0, "Redfish port override; 0 uses the mDNS-advertised port. Set (usually to 443) when browsing non-Redfish service types like _obmc_console._tcp.")
	flag.BoolVar(&insecureTLS, "insecure-tls", true, "Skip BMC TLS verification (BMCs commonly use self-signed certificates).")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Metrics endpoint bind address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe bind address.")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, bmcv1.AddToScheme, tinkv1.AddToScheme} {
		if err := add(scheme); err != nil {
			log.Error(err, "unable to build scheme")
			os.Exit(1)
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "tinkerbell-bmc-discovery-controller.discovery.tinkerbell.org",
		LeaderElectionNamespace: namespace,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{namespace: {}},
		},
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	worker := &controller.Worker{
		Client: mgr.GetClient(),
		Browser: &mdns.ZeroconfBrowser{
			Log:          ctrl.Log.WithName("mdns"),
			ServiceTypes: strings.Split(serviceTypes, ","),
			Domain:       mdnsDomain,
			Interval:     browseInterval,
			Window:       browseWindow,
		},
		Collector: &inventory.BMCLibCollector{
			Timeout: collectTimeout,
			Log:     ctrl.Log.WithName("inventory"),
		},
		Syncer: &syncpkg.Syncer{
			Client:      mgr.GetClient(),
			Namespace:   namespace,
			InsecureTLS: insecureTLS,
			Now:         time.Now,
			Log:         ctrl.Log.WithName("sync"),
		},
		CredentialsSecret: types.NamespacedName{Namespace: namespace, Name: credentialsSecret},
		ResyncInterval:    resyncInterval,
		RedfishPort:       redfishPort,
		Log:               ctrl.Log.WithName("worker"),
	}
	if err := mgr.Add(worker); err != nil {
		log.Error(err, "unable to add discovery worker")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
