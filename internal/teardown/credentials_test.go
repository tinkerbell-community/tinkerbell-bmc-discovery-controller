package teardown

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newCache(t *testing.T, objs ...client.Object) (*CredentialCache, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(teardownScheme(t)).WithObjects(objs...).Build()
	return &CredentialCache{Client: c, Namespace: ownNamespace}, c
}

func clusterKey() client.ObjectKey {
	return client.ObjectKey{Namespace: testNamespace, Name: clusterName}
}

func TestCredentialCacheEnsureAndRefresh(t *testing.T) {
	ctx := context.Background()
	cache, c := newCache(t, talosconfigSecret())

	if _, err := cache.Ensure(ctx, clusterKey()); err != nil {
		t.Fatal(err)
	}
	var cached corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: ownNamespace, Name: "talosconfig-tinkerbell-talos"}, &cached); err != nil {
		t.Fatal(err)
	}
	if string(cached.Data["talosconfig"]) != minimalTalosconfig {
		t.Errorf("cached data mismatch: %q", cached.Data["talosconfig"])
	}
	if cached.Labels[ClusterNameLabel] != clusterName || cached.Labels[ClusterNamespaceLabel] != testNamespace {
		t.Errorf("cached labels = %v", cached.Labels)
	}

	// Unchanged source: no rewrite.
	before := cached.ResourceVersion
	if _, err := cache.Ensure(ctx, clusterKey()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&cached), &cached); err != nil {
		t.Fatal(err)
	}
	if cached.ResourceVersion != before {
		t.Error("Ensure rewrote the cache for an unchanged source")
	}

	// Rotated source: refreshed.
	var source corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: clusterName + "-talosconfig"}, &source); err != nil {
		t.Fatal(err)
	}
	source.Data["talosconfig"] = []byte(minimalTalosconfig + "# rotated\n")
	if err := c.Update(ctx, &source); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Ensure(ctx, clusterKey()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&cached), &cached); err != nil {
		t.Fatal(err)
	}
	if string(cached.Data["talosconfig"]) == minimalTalosconfig {
		t.Error("Ensure did not refresh the cache after source rotation")
	}
}

func TestCredentialCacheEnsureMissingSourceIsNoop(t *testing.T) {
	cache, _ := newCache(t)
	if _, err := cache.Ensure(context.Background(), clusterKey()); err != nil {
		t.Fatalf("missing source must not error: %v", err)
	}
}

func TestCredentialCacheConfigFallback(t *testing.T) {
	ctx := context.Background()
	cached := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talosconfig-tinkerbell-talos",
			Namespace: ownNamespace,
			Labels:    map[string]string{ClusterNameLabel: clusterName, ClusterNamespaceLabel: testNamespace},
		},
		Data: map[string][]byte{"talosconfig": []byte(minimalTalosconfig)},
	}
	// Live secret gone (cluster-delete GC): the cached copy carries it.
	cache, _ := newCache(t, cached)
	cfg, err := cache.Config(ctx, clusterKey())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Context != "test" {
		t.Errorf("parsed context = %q", cfg.Context)
	}

	// Neither live nor cached: error surfaces (teardown degrades to the
	// timeout paths).
	empty, _ := newCache(t)
	if _, err := empty.Config(ctx, clusterKey()); err == nil {
		t.Error("expected an error with no credentials anywhere")
	}
}

func TestCredentialCacheGC(t *testing.T) {
	ctx := context.Background()
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talosconfig-tinkerbell-gone",
			Namespace: ownNamespace,
			Labels:    map[string]string{ClusterNameLabel: "gone", ClusterNamespaceLabel: testNamespace},
		},
	}
	active := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talosconfig-tinkerbell-talos",
			Namespace: ownNamespace,
			Labels:    map[string]string{ClusterNameLabel: clusterName, ClusterNamespaceLabel: testNamespace},
		},
	}
	// "talos" cluster still exists; "gone" has neither Cluster nor Machines.
	cache, c := newCache(t, stale, active, testCluster())

	if err := cache.GC(ctx); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(stale), &s); !apierrors.IsNotFound(err) {
		t.Errorf("stale cache not collected: err=%v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(active), &s); err != nil {
		t.Errorf("active cache must survive: %v", err)
	}
}

func TestCredentialCacheGCKeepsOrphanWithMachines(t *testing.T) {
	ctx := context.Background()
	orphan := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talosconfig-tinkerbell-talos",
			Namespace: ownNamespace,
			Labels:    map[string]string{ClusterNameLabel: clusterName, ClusterNamespaceLabel: testNamespace},
		},
	}
	// Cluster object already gone, but a qualifying Machine still tears
	// down: the credentials must survive until it is done.
	machine := machineFixture("cp-1", "10.0.0.1", true)
	cache, c := newCache(t, orphan, machine)

	if err := cache.GC(ctx); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(orphan), &s); err != nil {
		t.Errorf("cache with live qualifying machines must survive GC: %v", err)
	}
}
