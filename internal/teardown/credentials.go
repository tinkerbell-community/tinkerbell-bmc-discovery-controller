package teardown

import (
	"context"
	"fmt"

	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// talosconfigKey is the data key CABPT uses in <cluster>-talosconfig.
	talosconfigKey = "talosconfig"
	// sourceVersionAnnotation records the source secret's resourceVersion on
	// the cached copy, so unchanged sources skip the write.
	sourceVersionAnnotation = "teardown.tinkerbell.org/source-resource-version"
)

// CredentialCache keeps a component-owned copy of each cluster's
// talosconfig secret in C3's own namespace, outside the cluster's namespace
// and ownership graph, so teardown credentials survive Cluster finalization
// and owner-reference GC during whole-cluster delete. The copy holds
// cluster-admin Talos client credentials; C3's namespace must be treated
// with the same sensitivity as the CAPI cluster namespace.
type CredentialCache struct {
	client.Client
	// Namespace is C3's own deployment namespace, where copies live.
	Namespace string
}

// sourceSecretName is CABPT's talosconfig secret name for a cluster.
func sourceSecretName(cluster client.ObjectKey) client.ObjectKey {
	return client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name + "-talosconfig"}
}

// cachedSecretName names the component-owned copy in C3's namespace.
func cachedSecretName(cluster client.ObjectKey) string {
	return fmt.Sprintf("talosconfig-%s-%s", cluster.Namespace, cluster.Name)
}

// Ensure refreshes the cached copy from the live secret when the source
// exists and changed, reporting whether it wrote. A missing source is not
// an error — the cache keeps whatever it has.
func (c *CredentialCache) Ensure(ctx context.Context, cluster client.ObjectKey) (bool, error) {
	var source corev1.Secret
	if err := c.Get(ctx, sourceSecretName(cluster), &source); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	cached := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cachedSecretName(cluster),
			Namespace: c.Namespace,
			Labels: map[string]string{
				ClusterNameLabel:      cluster.Name,
				ClusterNamespaceLabel: cluster.Namespace,
			},
			Annotations: map[string]string{
				sourceVersionAnnotation: source.ResourceVersion,
			},
		},
		Data: map[string][]byte{talosconfigKey: source.Data[talosconfigKey]},
	}

	var existing corev1.Secret
	err := c.Get(ctx, client.ObjectKeyFromObject(cached), &existing)
	switch {
	case apierrors.IsNotFound(err):
		return true, c.Create(ctx, cached, client.FieldOwner(Name))
	case err != nil:
		return false, err
	case existing.Annotations[sourceVersionAnnotation] == source.ResourceVersion:
		return false, nil
	}
	cached.ResourceVersion = existing.ResourceVersion
	return true, c.Update(ctx, cached, client.FieldOwner(Name))
}

// Config returns the cluster's Talos client configuration, preferring the
// live secret and falling back to the cached copy.
func (c *CredentialCache) Config(ctx context.Context, cluster client.ObjectKey) (*talosconfig.Config, error) {
	var secret corev1.Secret
	err := c.Get(ctx, sourceSecretName(cluster), &secret)
	if apierrors.IsNotFound(err) {
		err = c.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: cachedSecretName(cluster)}, &secret)
	}
	if err != nil {
		return nil, err
	}
	cfg, err := talosconfig.FromBytes(secret.Data[talosconfigKey])
	if err != nil {
		return nil, fmt.Errorf("parsing talosconfig for cluster %s: %w", cluster, err)
	}
	return cfg, nil
}

// GC deletes cached copies whose Cluster no longer exists and for which no
// qualifying Machine remains. Run at startup and periodically.
func (c *CredentialCache) GC(ctx context.Context) error {
	log := logf.FromContext(ctx)

	var cachedSecrets corev1.SecretList
	if err := c.List(ctx, &cachedSecrets,
		client.InNamespace(c.Namespace), client.HasLabels{ClusterNameLabel}); err != nil {
		return err
	}
	for i := range cachedSecrets.Items {
		secret := &cachedSecrets.Items[i]
		cluster := client.ObjectKey{
			Namespace: secret.Labels[ClusterNamespaceLabel],
			Name:      secret.Labels[ClusterNameLabel],
		}
		stale, err := c.clusterGone(ctx, cluster)
		if err != nil {
			return err
		}
		if !stale {
			continue
		}
		log.Info("garbage-collecting cached talosconfig", "secret", secret.Name, "cluster", cluster)
		if err := c.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// clusterGone reports whether the Cluster is deleted AND no qualifying
// Machine of it remains.
func (c *CredentialCache) clusterGone(ctx context.Context, cluster client.ObjectKey) (bool, error) {
	var cl clusterv1.Cluster
	err := c.Get(ctx, cluster, &cl)
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	var machines clusterv1.MachineList
	if err := c.List(ctx, &machines, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return false, err
	}
	for i := range machines.Items {
		if Qualifies(&machines.Items[i]) {
			return false, nil
		}
	}
	return true, nil
}
