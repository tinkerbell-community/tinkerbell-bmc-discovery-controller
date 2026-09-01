package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bmc-toolbox/common"
	bmcv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/inventory"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
)

// Syncer upserts the resources describing one discovered BMC.
type Syncer struct {
	Client      client.Client
	Namespace   string
	InsecureTLS bool
	// NameTemplate, when set (e.g. "talos-${mac}"), names resources via
	// TemplateName; endpoints whose inventory cannot resolve it fall back
	// to the mDNS-derived ResourceName.
	NameTemplate string
	// FacilityCode fills metadata.facility.facility_code on Hardware.
	FacilityCode string
	// AutoEnrollment enables Tinkerbell auto enrollment on Hardware.
	AutoEnrollment bool
	Now            func() time.Time
	Log            *slog.Logger
}

// Sync upserts the auth Secret, Machine, and Hardware for an endpoint whose
// BMC connection has been verified — dev is the collected inventory and must
// not be nil, so resources only ever describe reachable, authenticated BMCs.
// Existing resources without the managed-by label are skipped, never
// modified. All writes are sparse server-side applies under FieldManager
// (create is a plain POST), so fields owned by other controllers — CAPT's
// Hardware spec.userData above all — are never touched
// (docs/discovery-field-ownership.md).
func (s *Syncer) Sync(ctx context.Context, ep mdns.Endpoint, dev *common.Device, creds inventory.Credentials) error {
	if dev == nil {
		return errors.New("refusing to sync without verified inventory")
	}
	name := TemplateName(s.NameTemplate, ep, dev)
	if name == "" {
		name = ResourceName(ep)
		if s.NameTemplate != "" {
			s.Log.Warn("name template unresolvable for endpoint; using mDNS-derived name",
				"template", s.NameTemplate, "name", name, "instance", ep.Instance)
		}
	}
	authName := name + "-bmc-auth"
	s.Log.Debug("syncing resources for verified BMC",
		"name", name, "instance", ep.Instance, "service", ep.Service, "host", ep.IP.String())

	secret := DesiredAuthSecret(authName, s.Namespace, creds)
	if err := s.applyManaged(ctx, "auth secret", &corev1.Secret{}, secret, ep, nil); err != nil {
		return fmt.Errorf("syncing auth secret %s: %w", authName, err)
	}

	machine := DesiredMachine(ep, name, s.Namespace, s.InsecureTLS, corev1.SecretReference{
		Name:      authName,
		Namespace: s.Namespace,
	})
	if err := s.applyManaged(ctx, "machine", &bmcv1.Machine{}, machine, ep, nil); err != nil {
		return fmt.Errorf("syncing machine %s: %w", name, err)
	}

	hardware := DesiredHardware(dev, HardwareOptions{
		Name:           name,
		Namespace:      s.Namespace,
		FacilityCode:   s.FacilityCode,
		AutoEnrollment: s.AutoEnrollment,
	}, nil)
	// Carry the live netboot values forward on update: the interfaces list
	// is SSA-atomic, so the apply must include netboot — with the value the
	// tink workflow controller last set, not discovery's create defaults.
	carry := func(live client.Object) {
		lh, ok := live.(*tinkv1.Hardware)
		if !ok || len(hardware.Spec.Interfaces) == 0 {
			return
		}
		hardware.Spec.Interfaces[0].Netboot = netbootFor(lh)
	}
	if err := s.applyManaged(ctx, "hardware", &tinkv1.Hardware{}, hardware, ep, carry); err != nil {
		return fmt.Errorf("syncing hardware %s: %w", name, err)
	}
	return nil
}

// applyManaged gets the live object into live (same concrete type as
// desired), enforces the managed-by adoption guard, and either Creates (not
// found) or server-side-applies (found and labeled) the desired sparse
// object under FieldManager. carry, when non-nil, is invoked with the live
// object before the apply so the caller can serialize foreign state it owns
// but does not author (Hardware netboot). The apply carries the live
// resourceVersion as an optimistic-concurrency precondition: a concurrent
// write between the Get and the apply surfaces as a Conflict error, and the
// worker requeues with rate-limited backoff.
func (s *Syncer) applyManaged(ctx context.Context, kind string, live, desired client.Object, ep mdns.Endpoint, carry func(live client.Object)) error {
	s.stampOwnership(desired, ep)
	log := s.Log.With("kind", kind, "name", desired.GetName(), "namespace", desired.GetNamespace())

	err := s.Client.Get(ctx, client.ObjectKeyFromObject(desired), live)
	switch {
	case apierrors.IsNotFound(err):
		// Create is a plain POST, not an apply-as-upsert: racing a
		// concurrent foreign creation must fail with AlreadyExists (requeue,
		// then guard skip) instead of silently adopting the object. It is
		// also the one point where netboot defaults are authored.
		if err := s.Client.Create(ctx, desired, client.FieldOwner(FieldManager)); err != nil {
			return err
		}
		log.Info("created resource")
		return nil
	case err != nil:
		return err
	}

	if live.GetLabels()[ManagedByLabel] != ManagedByValue {
		// Terraform-created and hand-provisioned resources are never
		// modified or adopted. The guard is evaluated against the same
		// revision the apply is conditioned on: a label stripped between
		// check and write surfaces as a Conflict, not a corruption.
		log.Warn("skipping resource not managed by this controller")
		return nil
	}
	if carry != nil {
		carry(live)
	}
	desired.SetResourceVersion(live.GetResourceVersion())
	applyConfig, err := applyConfigurationFor(desired)
	if err != nil {
		return err
	}
	// ForceOwnership is scoped to the fields present in the sparse apply
	// configuration — every one of them is discovery's by contract, and
	// force makes legacy-manager migration and the tink-controller
	// interfaces ownership ping-pong deterministic. Foreign fields are
	// structurally unreachable: they are never serialized.
	if err := s.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(applyConfig), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return err
	}
	log.Info("applied resource")
	return nil
}

// applyConfigurationFor converts the sparse desired object into the SSA
// payload it will be applied as: JSON-tag conversion plus removal of the
// zero-value noise typed objects carry (status — a subresource on both CRDs
// and never discovery's to assert — and metadata.creationTimestamp).
func applyConfigurationFor(desired client.Object) (*unstructured.Unstructured, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	if err != nil {
		return nil, fmt.Errorf("converting %T to apply configuration: %w", desired, err)
	}
	u := &unstructured.Unstructured{Object: content}
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return u, nil
}

// stampOwnership sets the discovery-owned label and annotations on the
// sparse desired object; maps are SSA-granular, so only these keys are
// asserted.
func (s *Syncer) stampOwnership(obj client.Object, ep mdns.Endpoint) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ManagedByLabel] = ManagedByValue
	obj.SetLabels(labels)

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[LastSeenAnnotation] = s.Now().UTC().Format(time.RFC3339)
	annotations[InstanceAnnotation] = ep.Instance
	annotations[ServiceAnnotation] = ep.Service
	obj.SetAnnotations(annotations)
}
