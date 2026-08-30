// Package controller wires mDNS discovery, inventory collection, and
// resource syncing into a controller-runtime Runnable.
package controller

import (
	"context"
	"fmt"
	stdsync "sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
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
	ResyncInterval    time.Duration
	Log               logr.Logger

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

	events := make(chan mdns.Endpoint, 16)
	go func() {
		if err := w.Browser.Run(ctx, events); err != nil {
			w.Log.Error(err, "mDNS browser stopped")
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
			w.known[ep.Key()] = ep
			w.mu.Unlock()
			queue.Add(ep.Key())
		case <-ticker.C:
			w.mu.Lock()
			for key := range w.known {
				queue.Add(key)
			}
			w.mu.Unlock()
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
			w.Log.Info("sync failed; will retry", "endpoint", key, "error", err.Error())
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

	creds, err := w.credentials(ctx)
	if err != nil {
		return err
	}

	dev, collectErr := w.Collector.Collect(ctx, ep, creds)
	if collectErr != nil {
		w.Log.Info("inventory collection failed; syncing connection-only Machine",
			"endpoint", key, "error", collectErr.Error())
		dev = nil
	}

	if err := w.Syncer.Sync(ctx, ep, dev, creds); err != nil {
		return err
	}
	// A successful Sync of a connection-only Machine still retries collection.
	return collectErr
}

func (w *Worker) credentials(ctx context.Context) (inventory.Credentials, error) {
	var secret corev1.Secret
	if err := w.Client.Get(ctx, w.CredentialsSecret, &secret); err != nil {
		return inventory.Credentials{}, fmt.Errorf("reading credentials secret %s: %w", w.CredentialsSecret, err)
	}
	username, password := secret.Data["username"], secret.Data["password"]
	if len(username) == 0 || len(password) == 0 {
		return inventory.Credentials{}, fmt.Errorf("credentials secret %s must contain username and password keys", w.CredentialsSecret)
	}
	return inventory.Credentials{Username: string(username), Password: string(password)}, nil
}
