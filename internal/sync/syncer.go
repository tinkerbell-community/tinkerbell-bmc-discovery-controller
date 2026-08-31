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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/inventory"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
)

// errUnmanaged signals that an existing resource lacks the managed-by label
// and must be left alone.
var errUnmanaged = errors.New("resource exists but is not managed by this controller")

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
// modified.
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

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: authName, Namespace: s.Namespace}}
	if err := s.upsert(ctx, "auth secret", secret, ep, func() {
		secret.Data = map[string][]byte{
			"username": []byte(creds.Username),
			"password": []byte(creds.Password),
		}
	}); err != nil {
		return fmt.Errorf("syncing auth secret %s: %w", authName, err)
	}

	machine := &bmcv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
	if err := s.upsert(ctx, "machine", machine, ep, func() {
		machine.Spec = DesiredMachineSpec(ep, s.InsecureTLS, corev1.SecretReference{
			Name:      authName,
			Namespace: s.Namespace,
		})
	}); err != nil {
		return fmt.Errorf("syncing machine %s: %w", name, err)
	}

	hardware := &tinkv1.Hardware{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
	if err := s.upsert(ctx, "hardware", hardware, ep, func() {
		hardware.Spec = DesiredHardwareSpec(dev, HardwareOptions{
			Name:           name,
			FacilityCode:   s.FacilityCode,
			AutoEnrollment: s.AutoEnrollment,
		})
	}); err != nil {
		return fmt.Errorf("syncing hardware %s: %w", name, err)
	}
	return nil
}

// upsert creates or updates obj, applying mutate plus the discovery labels
// and annotations. An existing object without the managed-by label is left
// untouched (logged, nil error).
func (s *Syncer) upsert(ctx context.Context, kind string, obj client.Object, ep mdns.Endpoint, mutate func()) error {
	op, err := controllerutil.CreateOrUpdate(ctx, s.Client, obj, func() error {
		// A non-empty resourceVersion means CreateOrUpdate's Get found an
		// existing object (creationTimestamp is not reliable with fake clients).
		if obj.GetResourceVersion() != "" && obj.GetLabels()[ManagedByLabel] != ManagedByValue {
			return errUnmanaged
		}
		mutate()
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
		return nil
	})
	log := s.Log.With("kind", kind, "name", obj.GetName(), "namespace", obj.GetNamespace())
	switch {
	case errors.Is(err, errUnmanaged):
		log.Warn("skipping resource not managed by this controller")
		return nil
	case err != nil:
		return err
	}
	switch op {
	case controllerutil.OperationResultCreated:
		log.Info("created resource")
	case controllerutil.OperationResultUpdated:
		log.Info("updated resource")
	default:
		log.Debug("resource up to date")
	}
	return nil
}
