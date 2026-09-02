// The talos-machine-teardown manager implements the CAPI pre-terminate
// machine deletion hook for Talos-on-Tinkerbell Machines: etcd membership
// removal for control-plane nodes and a Talos reset (wipe STATE+EPHEMERAL,
// halt) for all nodes, strictly before CAPT powers the hardware off. See
// docs/talos-machine-teardown.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/go-logr/logr"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/logging"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/teardown"
)

// credentialGCInterval paces the cached-talosconfig garbage collection.
const credentialGCInterval = 10 * time.Minute

// Build metadata injected by goreleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
	builtBy = "unknown"
)

// options carries the parsed flags.
type options struct {
	watchNamespace   string
	cacheNamespace   string
	etcdTimeout      time.Duration
	resetTimeout     time.Duration
	etcdCallTimeout  time.Duration
	resetCallTimeout time.Duration
	concurrency      int
	leaderElect      bool
	metricsAddr      string
	probeAddr        string
	logLevel         string
	logFormat        string
}

func registerFlags(o *options) {
	flag.StringVar(&o.watchNamespace, "watch-namespace", "", "Namespace to watch for CAPI objects; empty watches all namespaces.")
	flag.StringVar(&o.cacheNamespace, "cache-namespace", "", "Namespace for cached talosconfig secrets (the controller's own namespace); defaults to POD_NAMESPACE.")
	flag.DurationVar(&o.etcdTimeout, "etcd-timeout", 2*time.Minute, "etcd phase deadline, measured from the pre-terminate-observed-at annotation.")
	flag.DurationVar(&o.resetTimeout, "reset-timeout", 5*time.Minute, "Reset phase deadline, measured from the pre-terminate-observed-at annotation (includes the etcd phase).")
	flag.DurationVar(&o.etcdCallTimeout, "etcd-call-timeout", 10*time.Second, "Per-RPC timeout for Talos etcd operations.")
	flag.DurationVar(&o.resetCallTimeout, "reset-call-timeout", 30*time.Second, "Per-RPC timeout for the Talos reset.")
	flag.IntVar(&o.concurrency, "concurrency", 4, "Maximum concurrent reconciles (etcd work is serialized per cluster regardless).")
	flag.BoolVar(&o.leaderElect, "leader-elect", true, "Enable leader election.")
	flag.StringVar(&o.metricsAddr, "metrics-bind-address", ":8080", "Metrics endpoint bind address.")
	flag.StringVar(&o.probeAddr, "health-probe-bind-address", ":8081", "Health probe bind address.")
	flag.StringVar(&o.logLevel, "log-level", "info", "Log level: debug, info, warn, or error.")
	flag.StringVar(&o.logFormat, "log-format", "json", "Log format: json or text.")
}

func main() {
	var opts options
	registerFlags(&opts)
	flag.Parse()
	if opts.cacheNamespace == "" {
		opts.cacheNamespace = os.Getenv("POD_NAMESPACE")
	}

	root, err := logging.New(opts.logLevel, opts.logFormat, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctrl.SetLogger(logr.FromSlogHandler(root.Handler()))
	log := logging.Component(root, "setup")
	log.Info(teardown.Name, "version", version, "commit", commit, "date", date, "builtBy", builtBy)

	if err := run(opts, root, log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(opts options, root, log *slog.Logger) error {
	if opts.cacheNamespace == "" {
		return fmt.Errorf("--cache-namespace is required when POD_NAMESPACE is unset")
	}

	mgr, err := buildManager(opts)
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	credentials := &teardown.CredentialCache{Client: mgr.GetClient(), Namespace: opts.cacheNamespace}
	reconciler := &teardown.Reconciler{
		Client:           mgr.GetClient(),
		Recorder:         mgr.GetEventRecorder(teardown.Name),
		TalosFactory:     teardown.NewTalosClient,
		Credentials:      credentials,
		EtcdTimeout:      opts.etcdTimeout,
		ResetTimeout:     opts.resetTimeout,
		EtcdCallTimeout:  opts.etcdCallTimeout,
		ResetCallTimeout: opts.resetCallTimeout,
	}
	if err := reconciler.SetupWithManager(mgr, opts.concurrency); err != nil {
		return fmt.Errorf("setting up controller: %w", err)
	}
	if err := mgr.Add(credentialGC(credentials, root)); err != nil {
		return fmt.Errorf("adding credential GC: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up ready check: %w", err)
	}

	log.Info("starting manager")
	return mgr.Start(ctrl.SetupSignalHandler())
}

func buildManager(opts options) (ctrl.Manager, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme, tinkv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("building scheme: %w", err)
		}
	}

	cacheOptions := cache.Options{}
	if opts.watchNamespace != "" {
		cacheOptions.DefaultNamespaces = map[string]cache.Config{opts.watchNamespace: {}}
	}

	return ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: opts.metricsAddr},
		HealthProbeBindAddress:  opts.probeAddr,
		LeaderElection:          opts.leaderElect,
		LeaderElectionID:        "talos-machine-teardown.teardown.tinkerbell.org",
		LeaderElectionNamespace: opts.cacheNamespace,
		Cache:                   cacheOptions,
		Client: client.Options{
			// Secrets and Hardware are read directly, never through the
			// cache: the RBAC grants get/list only (no watch), a cached
			// Secret informer would hold every cluster secret in memory,
			// and the credential cache writes to the controller's own
			// namespace, which the CAPI-scoped cache does not cover.
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}, &tinkv1.Hardware{}},
			},
		},
	})
}

// credentialGC returns the periodic (and startup) garbage collector for
// cached talosconfig secrets whose cluster and machines are gone.
func credentialGC(credentials *teardown.CredentialCache, root *slog.Logger) manager.Runnable {
	return manager.RunnableFunc(func(ctx context.Context) error {
		log := logging.Component(root, "credential-gc")
		ticker := time.NewTicker(credentialGCInterval)
		defer ticker.Stop()
		for {
			if err := credentials.GC(ctx); err != nil {
				log.Warn("credential cache garbage collection failed", "err", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	})
}
