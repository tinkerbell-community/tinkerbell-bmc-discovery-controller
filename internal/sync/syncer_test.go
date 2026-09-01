package sync

import (
	"context"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/bmc-toolbox/common"

	bmcv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/inventory"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, bmcv1.AddToScheme, tinkv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func newTestSyncer(t *testing.T, objs ...client.Object) (*Syncer, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return &Syncer{
		Client:      c,
		Namespace:   "tink",
		InsecureTLS: true,
		Now:         func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) },
		Log:         slog.New(slog.DiscardHandler),
	}, c
}

var testCreds = inventory.Credentials{Username: "admin", Password: "secret"}

func TestSyncCreatesAll(t *testing.T) {
	s, c := newTestSyncer(t)
	ctx := context.Background()

	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t-bmc-auth"}, &secret); err != nil {
		t.Fatalf("auth secret: %v", err)
	}
	if string(secret.Data["username"]) != "admin" || string(secret.Data["password"]) != "secret" {
		t.Errorf("secret data = %v", secret.Data)
	}

	var machine bmcv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine); err != nil {
		t.Fatalf("machine: %v", err)
	}
	if machine.Labels[ManagedByLabel] != ManagedByValue {
		t.Errorf("machine labels = %v", machine.Labels)
	}
	if machine.Annotations[LastSeenAnnotation] != "2026-08-30T12:00:00Z" {
		t.Errorf("machine last-seen = %q", machine.Annotations[LastSeenAnnotation])
	}
	if machine.Annotations[InstanceAnnotation] != "X570D4I-2T" || machine.Annotations[ServiceAnnotation] != "_obmc_redfish._tcp" {
		t.Errorf("machine annotations = %v", machine.Annotations)
	}
	if machine.Spec.Connection.Host != "10.0.80.1" || machine.Spec.Connection.AuthSecretRef.Name != "x570d4i-2t-bmc-auth" {
		t.Errorf("machine spec = %+v", machine.Spec)
	}

	var hw tinkv1.Hardware
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &hw); err != nil {
		t.Fatalf("hardware: %v", err)
	}
	if hw.Labels[ManagedByLabel] != ManagedByValue {
		t.Errorf("hardware labels = %v", hw.Labels)
	}
	if hw.Spec.AgentID != "aa:bb:cc:dd:ee:01" || hw.Spec.BMCRef == nil || hw.Spec.BMCRef.Name != "x570d4i-2t" {
		t.Errorf("hardware spec = %+v", hw.Spec)
	}
}

func TestSyncNameTemplate(t *testing.T) {
	s, c := newTestSyncer(t)
	s.NameTemplate = "talos-${mac}"
	ctx := context.Background()

	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var machine bmcv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "talos-aabbccddee01"}, &machine); err != nil {
		t.Fatalf("templated machine name: %v", err)
	}
	var hw tinkv1.Hardware
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "talos-aabbccddee01"}, &hw); err != nil {
		t.Fatalf("templated hardware name: %v", err)
	}
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "talos-aabbccddee01-bmc-auth"}, &secret); err != nil {
		t.Fatalf("templated auth secret name: %v", err)
	}
}

func TestSyncNameTemplateFallsBack(t *testing.T) {
	// A device without MACs cannot resolve ${mac}; the hostname-derived name
	// is used instead so the BMC is still managed.
	s, c := newTestSyncer(t)
	s.NameTemplate = "talos-${mac}"
	ctx := context.Background()

	dev := common.NewDevice()
	dev.Serial = "SN99"
	if err := s.Sync(ctx, testEndpoint(), &dev, testCreds); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var machine bmcv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine); err != nil {
		t.Fatalf("fallback machine name: %v", err)
	}
}

func TestSyncRequiresInventory(t *testing.T) {
	// Resources are only created for BMCs with a verified connection, which
	// inventory collection proves; a nil device is a caller bug.
	s, c := newTestSyncer(t)
	ctx := context.Background()

	if err := s.Sync(ctx, testEndpoint(), nil, testCreds); err == nil {
		t.Fatal("Sync with nil device should error")
	}

	var machine bmcv1.Machine
	err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("machine should not exist, got err=%v", err)
	}
}

func TestSyncSkipsUnmanagedResources(t *testing.T) {
	existing := &bmcv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "x570d4i-2t", Namespace: "tink"},
		Spec: bmcv1.MachineSpec{
			Connection: bmcv1.Connection{Host: "192.168.1.99"},
		},
	}
	s, c := newTestSyncer(t, existing)
	ctx := context.Background()

	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var machine bmcv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine); err != nil {
		t.Fatal(err)
	}
	if machine.Spec.Connection.Host != "192.168.1.99" {
		t.Errorf("unmanaged machine was modified: %+v", machine.Spec)
	}
	if _, ok := machine.Labels[ManagedByLabel]; ok {
		t.Errorf("unmanaged machine was adopted: %v", machine.Labels)
	}
}

func TestSyncUpdatesExistingManaged(t *testing.T) {
	s, c := newTestSyncer(t)
	ctx := context.Background()

	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}

	// Second sync: endpoint moved and time advanced.
	s.Now = func() time.Time { return time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC) }
	ep := testEndpoint()
	ep.IP = netip.MustParseAddr("10.0.80.2")
	if err := s.Sync(ctx, ep, testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}

	var machine bmcv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine); err != nil {
		t.Fatal(err)
	}
	if machine.Spec.Connection.Host != "10.0.80.2" {
		t.Errorf("machine host = %q, want 10.0.80.2", machine.Spec.Connection.Host)
	}
	if machine.Annotations[LastSeenAnnotation] != "2026-08-30T13:00:00Z" {
		t.Errorf("last-seen = %q", machine.Annotations[LastSeenAnnotation])
	}
}

func TestSyncCarriesNetbootForward(t *testing.T) {
	// The tink workflow controller disarms PXE after a workflow completes;
	// discovery's resync must reproduce that value verbatim, never re-arm it
	// (create-only netboot authorship, docs/discovery-field-ownership.md).
	s, c := newTestSyncer(t)
	ctx := context.Background()

	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}

	var hw tinkv1.Hardware
	key := types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}
	if err := c.Get(ctx, key, &hw); err != nil {
		t.Fatal(err)
	}
	if nb := hw.Spec.Interfaces[0].Netboot; nb.AllowPXE == nil || !*nb.AllowPXE {
		t.Fatalf("create should author allowPXE=true, got %+v", nb)
	}

	// Simulate the tink workflow controller's post-workflow disarm.
	hw.Spec.Interfaces[0].Netboot.AllowPXE = ptr.To(false)
	if err := c.Update(ctx, &hw); err != nil {
		t.Fatal(err)
	}

	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &hw); err != nil {
		t.Fatal(err)
	}
	if nb := hw.Spec.Interfaces[0].Netboot; nb == nil || nb.AllowPXE == nil || *nb.AllowPXE {
		t.Errorf("resync re-armed PXE: netboot = %+v, want allowPXE false carried forward", nb)
	}
}

func TestSyncPreservesForeignHardwareFields(t *testing.T) {
	// CAPT writes spec.userData (the machine's Talos config source) and
	// talos-os-metadata writes spec.metadata.instance.operating_system;
	// discovery's sparse apply must leave both untouched. The authoritative
	// ownership check runs against a real API server (envtest, M3); this
	// locks in the behavior under the fake client's SSA emulation.
	s, c := newTestSyncer(t)
	ctx := context.Background()

	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}

	var hw tinkv1.Hardware
	key := types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}
	if err := c.Get(ctx, key, &hw); err != nil {
		t.Fatal(err)
	}
	hw.Spec.UserData = ptr.To("#cloud-config foreign")
	hw.Spec.Metadata.Instance.OperatingSystem = &tinkv1.MetadataInstanceOperatingSystem{Slug: "abc123"}
	hw.Spec.Metadata.Instance.State = "provisioned"
	if err := c.Update(ctx, &hw); err != nil {
		t.Fatal(err)
	}

	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &hw); err != nil {
		t.Fatal(err)
	}
	if hw.Spec.UserData == nil || *hw.Spec.UserData != "#cloud-config foreign" {
		t.Errorf("resync wiped spec.userData: %v", hw.Spec.UserData)
	}
	inst := hw.Spec.Metadata.Instance
	if inst == nil || inst.OperatingSystem == nil || inst.OperatingSystem.Slug != "abc123" {
		t.Errorf("resync wiped operating_system: %+v", inst)
	}
	if inst != nil && inst.State != "provisioned" {
		t.Errorf("resync wiped instance state: %q", inst.State)
	}
}
