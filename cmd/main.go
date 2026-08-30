// The tinkerbell-bmc-discovery-controller manager discovers BMCs over mDNS
// and maintains Tinkerbell Machine and Hardware resources from their
// Redfish inventory.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	bmcv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/controller"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/inventory"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/logging"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
	syncpkg "github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/sync"
)

// splitNonEmpty splits a comma-separated list, dropping empty elements so an
// unset flag yields nil rather than [""].
func splitNonEmpty(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Build metadata injected by goreleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
	builtBy = "unknown"
)

func main() {
	var (
		namespace         string
		serviceTypes      string
		mdnsDomain        string
		mdnsInterfaces    string
		browseInterval    time.Duration
		browseWindow      time.Duration
		resyncInterval    time.Duration
		collectTimeout    time.Duration
		credentialsSecret string
		defaultCreds      string
		redfishPort       int
		insecureTLS       bool
		leaderElect       bool
		metricsAddr       string
		probeAddr         string
		logLevel          string
		logFormat         string
	)
	flag.StringVar(&namespace, "namespace", "tink", "Namespace for created resources and the credentials secret.")
	flag.StringVar(&serviceTypes, "service-types", "_redfish._tcp,_obmc_redfish._tcp", "Comma-separated DNS-SD service types to browse.")
	flag.StringVar(&mdnsDomain, "mdns-domain", "local.", "mDNS browse domain.")
	flag.StringVar(&mdnsInterfaces, "mdns-interfaces", "", "Comma-separated interface names to browse on (e.g. net1 for a Multus attachment); empty browses all multicast-capable interfaces.")
	flag.DurationVar(&browseInterval, "browse-interval", 5*time.Minute, "Time between mDNS browse cycles.")
	flag.DurationVar(&browseWindow, "browse-window", 30*time.Second, "Duration of each mDNS browse cycle.")
	flag.DurationVar(&resyncInterval, "resync-interval", time.Hour, "Inventory refresh interval for known BMCs.")
	flag.DurationVar(&collectTimeout, "collect-timeout", 2*time.Minute, "Timeout for one inventory collection.")
	flag.StringVar(&credentialsSecret, "credentials-secret", "bmc-discovery-credentials", "Name of the Secret holding BMC username/password keys.")
	flag.StringVar(&defaultCreds, "default-credentials", "_obmc_console._tcp=root:0penBmc", "Per-service-type fallback credentials (<service>=<user>:<pass>, comma-separated) used when the credentials secret does not exist. Empty disables fallbacks.")
	flag.IntVar(&redfishPort, "redfish-port", 0, "Redfish port override; 0 uses the mDNS-advertised port. Set (usually to 443) when browsing non-Redfish service types like _obmc_console._tcp.")
	flag.BoolVar(&insecureTLS, "insecure-tls", true, "Skip BMC TLS verification (BMCs commonly use self-signed certificates).")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Metrics endpoint bind address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe bind address.")
	flag.StringVar(&logLevel, "log-level", "info", "Log level: debug, info, warn, or error.")
	flag.StringVar(&logFormat, "log-format", "json", "Log format: json or text.")
	flag.Parse()

	root, err := logging.New(logLevel, logFormat, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// controller-runtime and bmclib speak logr; bridge them onto slog. No
	// Component wrapper here: logr's WithName renders as the "logger"
	// attribute already, so wrapping would duplicate the key.
	ctrl.SetLogger(logr.FromSlogHandler(root.Handler()))
	log := logging.Component(root, "setup")
	log.Info("tinkerbell-bmc-discovery-controller", "version", version, "commit", commit, "date", date, "builtBy", builtBy)

	defaultCredentials, err := inventory.ParseDefaults(defaultCreds)
	if err != nil {
		log.Error("invalid --default-credentials", "err", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, bmcv1.AddToScheme, tinkv1.AddToScheme} {
		if err := add(scheme); err != nil {
			log.Error("unable to build scheme", "err", err)
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
		log.Error("unable to create manager", "err", err)
		os.Exit(1)
	}

	worker := &controller.Worker{
		Client: mgr.GetClient(),
		Browser: &mdns.ZeroconfBrowser{
			Log:          logging.Component(root, "mdns"),
			ServiceTypes: strings.Split(serviceTypes, ","),
			Domain:       mdnsDomain,
			Interval:     browseInterval,
			Window:       browseWindow,
			Interfaces:   splitNonEmpty(mdnsInterfaces),
		},
		Collector: &inventory.BMCLibCollector{
			Timeout: collectTimeout,
			Log:     logging.Component(root, "inventory"),
		},
		Syncer: &syncpkg.Syncer{
			Client:      mgr.GetClient(),
			Namespace:   namespace,
			InsecureTLS: insecureTLS,
			Now:         time.Now,
			Log:         logging.Component(root, "sync"),
		},
		CredentialsSecret:  types.NamespacedName{Namespace: namespace, Name: credentialsSecret},
		DefaultCredentials: defaultCredentials,
		ResyncInterval:     resyncInterval,
		RedfishPort:        redfishPort,
		Log:                logging.Component(root, "worker"),
	}
	if err := mgr.Add(worker); err != nil {
		log.Error("unable to add discovery worker", "err", err)
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error("unable to set up health check", "err", err)
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error("unable to set up ready check", "err", err)
		os.Exit(1)
	}

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error("manager exited with error", "err", err)
		os.Exit(1)
	}
}
