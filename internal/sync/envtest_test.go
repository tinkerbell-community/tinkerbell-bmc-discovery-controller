//go:build envtest

package sync

// Managed-fields semantics are only trustworthy against a real API server —
// the fake client's SSA emulation has diverged from apiserver behavior
// historically — so the field-ownership guarantees of the C0 migration
// (docs/discovery-field-ownership.md) are asserted here under envtest.
// Run with: make test-envtest

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	bmcv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var envClient client.WithWatch

func TestMain(m *testing.M) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "test", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, bmcv1.AddToScheme, tinkv1.AddToScheme} {
		if err := add(scheme); err != nil {
			fmt.Fprintf(os.Stderr, "building scheme: %v\n", err)
			os.Exit(1)
		}
	}
	envClient, err = client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "building client: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

// newEnvSyncer returns a Syncer against a fresh namespace so tests are
// isolated despite sharing one API server.
func newEnvSyncer(t *testing.T, c client.Client) *Syncer {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "sync-envtest-"}}
	if err := envClient.Create(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	if c == nil {
		c = envClient
	}
	return &Syncer{
		Client:      c,
		Namespace:   ns.Name,
		InsecureTLS: true,
		Now:         func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) },
		Log:         slog.New(slog.DiscardHandler),
	}
}

func hardwareKey(s *Syncer) types.NamespacedName {
	return types.NamespacedName{Namespace: s.Namespace, Name: "x570d4i-2t"}
}

func getHardware(t *testing.T, s *Syncer) *tinkv1.Hardware {
	t.Helper()
	var hw tinkv1.Hardware
	if err := envClient.Get(context.Background(), hardwareKey(s), &hw); err != nil {
		t.Fatal(err)
	}
	return &hw
}

// TestEnvtestForeignFieldSurvival is scenario 1: CAPT's userData
// (optimistic-lock Update), talos-os-metadata's operating_system (SSA under
// its own manager), and tink-controller's allowPXE=false disarm (Update)
// must all survive repeated discovery syncs.
func TestEnvtestForeignFieldSurvival(t *testing.T) {
	ctx := context.Background()
	s := newEnvSyncer(t, nil)
	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}

	// CAPT-style write: plain Update of spec.userData.
	hw := getHardware(t, s)
	hw.Spec.UserData = ptr.To("#cloud-config foreign")
	if err := envClient.Update(ctx, hw); err != nil {
		t.Fatal(err)
	}

	// talos-os-metadata-style write: sparse SSA under its own field manager.
	osPatch := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tinkerbell.org/v1alpha1",
		"kind":       "Hardware",
		"metadata":   map[string]any{"name": hw.Name, "namespace": hw.Namespace},
		"spec": map[string]any{
			"metadata": map[string]any{"instance": map[string]any{
				"operating_system": map[string]any{"slug": "abc123", "distro": "talos"},
			}},
		},
	}}
	if err := envClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(osPatch),
		client.FieldOwner("talos-os-metadata"), client.ForceOwnership); err != nil {
		t.Fatal(err)
	}

	// tink-controller-style write: Update disarming PXE after a workflow.
	hw = getHardware(t, s)
	hw.Spec.Interfaces[0].Netboot.AllowPXE = ptr.To(false)
	if err := envClient.Update(ctx, hw); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
			t.Fatal(err)
		}
	}

	hw = getHardware(t, s)
	if hw.Spec.UserData == nil || *hw.Spec.UserData != "#cloud-config foreign" {
		t.Errorf("sync wiped CAPT's spec.userData: %v", hw.Spec.UserData)
	}
	inst := hw.Spec.Metadata.Instance
	if inst == nil || inst.OperatingSystem == nil || inst.OperatingSystem.Slug != "abc123" {
		t.Errorf("sync wiped talos-os-metadata's operating_system: %+v", inst)
	}
	if nb := hw.Spec.Interfaces[0].Netboot; nb == nil || nb.AllowPXE == nil || *nb.AllowPXE {
		t.Errorf("sync re-armed PXE: %+v, want allowPXE false carried forward", nb)
	}
}

// TestEnvtestLegacyAdoption is scenario 2: a Hardware created by the
// pre-C0 code (full spec under a legacy Update manager) is taken over
// field-by-field on the first Apply; unasserted legacy fields stay put.
func TestEnvtestLegacyAdoption(t *testing.T) {
	ctx := context.Background()
	s := newEnvSyncer(t, nil)

	legacy := DesiredHardware(testDevice(), HardwareOptions{
		Name: "x570d4i-2t", Namespace: s.Namespace,
	}, nil)
	legacy.Labels = map[string]string{ManagedByLabel: ManagedByValue}
	legacy.Spec.UserData = ptr.To("#cloud-config legacy")
	if err := envClient.Create(ctx, legacy, client.FieldOwner("manager")); err != nil {
		t.Fatal(err)
	}

	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}

	hw := getHardware(t, s)
	var applied bool
	for _, mf := range hw.ManagedFields {
		if mf.Manager == FieldManager && mf.Operation == metav1.ManagedFieldsOperationApply {
			applied = true
		}
	}
	if !applied {
		t.Errorf("no Apply managed-fields entry for %q: %+v", FieldManager, hw.ManagedFields)
	}
	if hw.Spec.UserData == nil || *hw.Spec.UserData != "#cloud-config legacy" {
		t.Errorf("adoption removed the legacy manager's unasserted userData: %v", hw.Spec.UserData)
	}
	if hw.Annotations[LastSeenAnnotation] == "" {
		t.Error("adopted Hardware missing discovery annotations")
	}
}

// TestEnvtestGuard is scenario 3: unlabeled resources are never modified and
// never adopted.
func TestEnvtestGuard(t *testing.T) {
	ctx := context.Background()
	s := newEnvSyncer(t, nil)

	foreign := &bmcv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "x570d4i-2t", Namespace: s.Namespace},
		Spec:       bmcv1.MachineSpec{Connection: bmcv1.Connection{Host: "192.168.1.99", AuthSecretRef: corev1.SecretReference{Name: "keep"}}},
	}
	if err := envClient.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}

	var machine bmcv1.Machine
	if err := envClient.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: "x570d4i-2t"}, &machine); err != nil {
		t.Fatal(err)
	}
	if machine.Spec.Connection.Host != "192.168.1.99" {
		t.Errorf("unmanaged machine was modified: %+v", machine.Spec)
	}
	if _, ok := machine.Labels[ManagedByLabel]; ok {
		t.Errorf("unmanaged machine was adopted: %v", machine.Labels)
	}
}

// TestEnvtestConflictPrecondition is scenario 4: a write landing between
// discovery's Get and its Apply surfaces as a Conflict error, and the rerun
// succeeds while preserving the interleaved foreign write.
func TestEnvtestConflictPrecondition(t *testing.T) {
	ctx := context.Background()

	var raceOnce bool
	racing := interceptor.NewClient(envClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			hw, ok := obj.(*tinkv1.Hardware)
			if !ok || raceOnce {
				return nil
			}
			raceOnce = true
			// Interleave a foreign write after the Get returns the (now
			// stale) revision the apply will be conditioned on.
			fresh := hw.DeepCopy()
			if fresh.Annotations == nil {
				fresh.Annotations = map[string]string{}
			}
			fresh.Annotations["foreign.example.com/interleaved"] = "yes"
			return c.Update(ctx, fresh)
		},
	})
	s := newEnvSyncer(t, racing)

	// First sync creates everything (no Get race: NotFound path).
	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}
	// Second sync hits the interleaved write: expect a Conflict error.
	err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds)
	if err == nil {
		t.Fatal("expected a conflict from the resourceVersion precondition")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected Conflict, got %v", err)
	}
	// Rerun (interceptor now inert) succeeds and preserves the foreign write.
	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}
	hw := getHardware(t, s)
	if hw.Annotations["foreign.example.com/interleaved"] != "yes" {
		t.Errorf("interleaved foreign annotation lost: %v", hw.Annotations)
	}
}

// TestEnvtestCreatePath is scenario 5: fresh objects get netboot defaults,
// the managed-by label, the discovery annotations, and managed-fields
// entries under the component field manager.
func TestEnvtestCreatePath(t *testing.T) {
	ctx := context.Background()
	s := newEnvSyncer(t, nil)
	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}

	hw := getHardware(t, s)
	if hw.Labels[ManagedByLabel] != ManagedByValue {
		t.Errorf("labels = %v", hw.Labels)
	}
	for _, key := range []string{LastSeenAnnotation, InstanceAnnotation, ServiceAnnotation} {
		if hw.Annotations[key] == "" {
			t.Errorf("missing annotation %s: %v", key, hw.Annotations)
		}
	}
	nb := hw.Spec.Interfaces[0].Netboot
	if nb == nil || nb.AllowPXE == nil || !*nb.AllowPXE || nb.AllowWorkflow == nil || !*nb.AllowWorkflow {
		t.Errorf("netboot = %+v, want create defaults true/true", nb)
	}
	var managed bool
	for _, mf := range hw.ManagedFields {
		if mf.Manager == FieldManager {
			managed = true
		}
	}
	if !managed {
		t.Errorf("no managed-fields entry for %q: %+v", FieldManager, hw.ManagedFields)
	}
}

// TestEnvtestSecretRotation is scenario 6: a rotated password propagates,
// and a data key discovery previously asserted but no longer does is pruned
// (discovery owns its keys).
func TestEnvtestSecretRotation(t *testing.T) {
	ctx := context.Background()
	s := newEnvSyncer(t, nil)
	if err := s.Sync(ctx, testEndpoint(), testDevice(), testCreds); err != nil {
		t.Fatal(err)
	}

	// Simulate an older discovery version having asserted an extra data key
	// under the same field manager.
	stale := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "x570d4i-2t-bmc-auth", "namespace": s.Namespace},
		"data":       map[string]any{"token": "c3RhbGU="},
	}}
	if err := envClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(stale),
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		t.Fatal(err)
	}

	rotated := testCreds
	rotated.Password = "rotated"
	if err := s.Sync(ctx, testEndpoint(), testDevice(), rotated); err != nil {
		t.Fatal(err)
	}

	var secret corev1.Secret
	if err := envClient.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: "x570d4i-2t-bmc-auth"}, &secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["password"]) != "rotated" {
		t.Errorf("password = %q, want rotated", secret.Data["password"])
	}
	if _, ok := secret.Data["token"]; ok {
		t.Errorf("stale data key not pruned: %v", secret.Data)
	}
}
