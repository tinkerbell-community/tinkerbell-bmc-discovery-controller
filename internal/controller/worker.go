// Package controller wires mDNS discovery, inventory collection, and
// resource syncing into a controller-runtime Runnable.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"

	"github.com/bmc-toolbox/common"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/inventory"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
	syncpkg "github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/sync"
)

// Worker consumes discovered endpoints, collects their inventory, and syncs
// Tinkerbell resources. It implements manager.Runnable and only runs on the
// leader.
type Worker struct {
	Client            client.Client
	Browser           mdns.Browser
	Collector         inventory.Collector
	Syncer            *syncpkg.Syncer
	CredentialsSecret types.NamespacedName
	// DefaultCredentials supplies known default credentials keyed by DNS-SD
	// service type (e.g. OpenBMC's factory root/0penBmc for _obmc_*), with
	// inventory.WildcardService as the catch-all. They form the candidate
	// chain after the credentials Secret: the worker pivots through
	// candidates until one authenticates against the BMC.
	DefaultCredentials map[string]inventory.Credentials
	ResyncInterval     time.Duration
	// RedfishPort, when non-zero, replaces the mDNS-advertised port. Needed
	// when discovery rides a non-Redfish advertisement (e.g.
	// _obmc_console._tcp announces the SOL console port, not Redfish).
	RedfishPort int
	Log         *slog.Logger

	mu    stdsync.Mutex
	known map[string]mdns.Endpoint
}

// NeedLeaderElection makes the manager run the worker only on the leader.
func (w *Worker) NeedLeaderElection() bool {
	return true
}

// Start runs discovery until ctx is done. Failed endpoints are retried with
// rate-limited backoff; all known endpoints re-sync every ResyncInterval.
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	w.known = map[string]mdns.Endpoint{}
	w.mu.Unlock()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	defer queue.ShutDown()

	w.Log.Info("starting BMC discovery", "resyncInterval", w.ResyncInterval.String())
	events := make(chan mdns.Endpoint, 16)
	go func() {
		if err := w.Browser.Run(ctx, events); err != nil {
			w.Log.Error("mDNS browser stopped", "err", err)
		}
	}()
	go w.processQueue(ctx, queue)

	ticker := time.NewTicker(w.ResyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ep := <-events:
			w.mu.Lock()
			_, seen := w.known[ep.Key()]
			w.known[ep.Key()] = ep
			w.mu.Unlock()
			if seen {
				w.Log.Debug("BMC re-observed via mDNS", "endpoint", ep.Key(), "host", ep.IP.String())
			} else {
				w.Log.Info("new BMC discovered via mDNS",
					"endpoint", ep.Key(), "instance", ep.Instance, "service", ep.Service,
					"hostname", ep.Hostname, "host", ep.IP.String(), "port", ep.Port)
			}
			queue.Add(ep.Key())
		case <-ticker.C:
			w.mu.Lock()
			n := len(w.known)
			for key := range w.known {
				queue.Add(key)
			}
			w.mu.Unlock()
			w.Log.Debug("periodic resync of known BMCs", "count", n)
		}
	}
}

func (w *Worker) processQueue(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string]) {
	for {
		key, shutdown := queue.Get()
		if shutdown {
			return
		}
		if err := w.handle(ctx, key); err != nil {
			w.Log.Warn("sync failed; will retry with backoff", "endpoint", key, "err", err)
			queue.AddRateLimited(key)
		} else {
			queue.Forget(key)
		}
		queue.Done(key)
	}
}

func (w *Worker) handle(ctx context.Context, key string) error {
	w.mu.Lock()
	ep, ok := w.known[key]
	w.mu.Unlock()
	if !ok {
		return nil
	}
	if w.RedfishPort != 0 {
		ep.Port = w.RedfishPort
	}

	log := w.Log.With("endpoint", key, "host", ep.IP.String(), "port", ep.Port)
	log.Debug("handling BMC endpoint")

	candidates := w.credentialCandidates(ctx, ep)
	if len(candidates) == 0 {
		return fmt.Errorf("no credentials available: secret %s is absent or malformed and no default credentials are configured", w.CredentialsSecret)
	}

	// Collect doubles as connection verification: resources are only
	// created for BMCs we can authenticate to and inventory. The candidate
	// chain pivots through known credentials until one authenticates.
	var dev *common.Device
	var creds inventory.Credentials
	var lastErr error
	for _, candidate := range candidates {
		log.Debug("verifying BMC connection and collecting inventory",
			"credentialSource", candidate.source, "username", candidate.creds.Username)
		d, err := w.Collector.Collect(ctx, ep, candidate.creds)
		if err != nil {
			lastErr = err
			log.Debug("BMC verification failed with candidate credentials",
				"credentialSource", candidate.source, "err", err)
			continue
		}
		dev, creds = d, candidate.creds
		log.Info("BMC connection verified",
			"credentialSource", candidate.source, "vendor", dev.Vendor, "model", dev.Model, "serial", dev.Serial)
		break
	}
	if dev == nil {
		log.Warn("BMC connection could not be verified with any candidate credentials; deferring resource creation",
			"candidatesTried", len(candidates), "err", lastErr)
		return lastErr
	}

	return w.Syncer.Sync(ctx, ep, dev, creds)
}

// credentialCandidate pairs credentials with their origin, for logging.
type credentialCandidate struct {
	creds  inventory.Credentials
	source string
}

// credentialCandidates builds the ordered chain to try against a BMC: the
// credentials Secret when present and well-formed, then the service-type
// default, then the wildcard default. Duplicate pairs are collapsed.
func (w *Worker) credentialCandidates(ctx context.Context, ep mdns.Endpoint) []credentialCandidate {
	var candidates []credentialCandidate
	add := func(creds inventory.Credentials, source string) {
		for _, existing := range candidates {
			if existing.creds == creds {
				return
			}
		}
		candidates = append(candidates, credentialCandidate{creds: creds, source: source})
	}

	var secret corev1.Secret
	switch err := w.Client.Get(ctx, w.CredentialsSecret, &secret); {
	case err == nil:
		username, password := secret.Data["username"], secret.Data["password"]
		if len(username) > 0 && len(password) > 0 {
			add(inventory.Credentials{Username: string(username), Password: string(password)}, "secret")
		} else {
			w.Log.Warn("credentials secret is missing username/password keys; relying on default credentials",
				"secret", w.CredentialsSecret.String())
		}
	case apierrors.IsNotFound(err):
		w.Log.Debug("credentials secret not found; relying on default credentials",
			"secret", w.CredentialsSecret.String())
	default:
		w.Log.Warn("failed to read credentials secret; relying on default credentials",
			"secret", w.CredentialsSecret.String(), "err", err)
	}

	if creds, ok := w.DefaultCredentials[ep.Service]; ok {
		add(creds, "service default")
	}
	if creds, ok := w.DefaultCredentials[inventory.WildcardService]; ok {
		add(creds, "global default")
	}
	return candidates
}
