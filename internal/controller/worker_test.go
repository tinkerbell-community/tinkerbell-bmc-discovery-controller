package controller

import (
	"context"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/inventory"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
	syncpkg "github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/sync"
)

type fakeBrowser struct {
	endpoints []mdns.Endpoint
}

func (b *fakeBrowser) Run(ctx context.Context, events chan<- mdns.Endpoint) error {
	for _, ep := range b.endpoints {
		select {
		case events <- ep:
		case <-ctx.Done():
			return nil
		}
	}
	<-ctx.Done()
	return nil
}

type fakeCollector struct {
	dev *common.Device
	err error
	// accept, when set, simulates BMC authentication: Collect succeeds only
	// for these exact credentials and fails for any others.
	accept *inventory.Credentials
}

func (c *fakeCollector) Collect(_ context.Context, _ mdns.Endpoint, creds inventory.Credentials) (*common.Device, error) {
	if c.accept != nil && creds != *c.accept {
		return nil, errors.New("authentication failed")
	}
	return c.dev, c.err
}

func testEndpoint() mdns.Endpoint {
	return mdns.Endpoint{
		Instance: "X570D4I-2T",
		Service:  "_obmc_redfish._tcp",
		Hostname: "X570D4I-2T.local.",
		IP:       netip.MustParseAddr("10.0.80.1"),
		Port:     443,
	}
}

func credsSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-discovery-credentials", Namespace: "tink"},
		Data:       data,
	}
}

func newWorker(t *testing.T, browser mdns.Browser, collector inventory.Collector, objs ...client.Object) (*Worker, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, bmcv1.AddToScheme, tinkv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	discard := slog.New(slog.DiscardHandler)
	return &Worker{
		Client:    c,
		Browser:   browser,
		Collector: collector,
		Syncer: &syncpkg.Syncer{
			Client:      c,
			Namespace:   "tink",
			InsecureTLS: true,
			Now:         time.Now,
			Log:         discard,
		},
		CredentialsSecret: types.NamespacedName{Namespace: "tink", Name: "bmc-discovery-credentials"},
		ResyncInterval:    time.Hour,
		Log:               discard,
	}, c
}

// eventually polls until check returns true or the deadline passes.
func eventually(t *testing.T, check func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return check()
}

func TestWorkerSyncsDiscoveredEndpoint(t *testing.T) {
	dev := common.NewDevice()
	dev.Serial = "SN1"
	dev.NICs = []*common.NIC{{NICPorts: []*common.NICPort{{MacAddress: "aa:bb:cc:dd:ee:01"}}}}

	w, c := newWorker(t,
		&fakeBrowser{endpoints: []mdns.Endpoint{testEndpoint()}},
		&fakeCollector{dev: &dev},
		credsSecret(map[string][]byte{"username": []byte("admin"), "password": []byte("pw")}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	ok := eventually(t, func() bool {
		var machine bmcv1.Machine
		var hw tinkv1.Hardware
		mErr := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine)
		hErr := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &hw)
		return mErr == nil && hErr == nil
	})
	if !ok {
		t.Fatal("machine and hardware were not created")
	}
}

func TestWorkerCollectFailureCreatesNothing(t *testing.T) {
	// An unverifiable BMC connection must not produce any resources.
	w, c := newWorker(t,
		&fakeBrowser{endpoints: []mdns.Endpoint{testEndpoint()}},
		&fakeCollector{err: errors.New("bmc unreachable")},
		credsSecret(map[string][]byte{"username": []byte("admin"), "password": []byte("pw")}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	time.Sleep(300 * time.Millisecond)
	var machine bmcv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine); !apierrors.IsNotFound(err) {
		t.Fatalf("machine should not exist without a verified connection, got err=%v", err)
	}
	var hw tinkv1.Hardware
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &hw); !apierrors.IsNotFound(err) {
		t.Fatalf("hardware should not exist without a verified connection, got err=%v", err)
	}
}

func TestWorkerDefaultCredentials(t *testing.T) {
	// With no credentials Secret, the per-service-type default applies.
	dev := common.NewDevice()
	dev.NICs = []*common.NIC{{NICPorts: []*common.NICPort{{MacAddress: "aa:bb:cc:dd:ee:01"}}}}

	w, c := newWorker(t,
		&fakeBrowser{endpoints: []mdns.Endpoint{testEndpoint()}},
		&fakeCollector{dev: &dev},
	)
	w.DefaultCredentials = map[string]inventory.Credentials{
		"_obmc_redfish._tcp": {Username: "root", Password: "0penBmc"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	if !eventually(t, func() bool {
		var machine bmcv1.Machine
		return c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine) == nil
	}) {
		t.Fatal("machine was not created with default credentials")
	}
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t-bmc-auth"}, &secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["username"]) != "root" || string(secret.Data["password"]) != "0penBmc" {
		t.Errorf("auth secret data = %v, want root/0penBmc", secret.Data)
	}
}

func TestWorkerRedfishPortOverride(t *testing.T) {
	// A BMC discovered via a non-Redfish advertisement (e.g.
	// _obmc_console._tcp on port 2200) still gets a Machine pointing at the
	// configured Redfish port.
	ep := testEndpoint()
	ep.Service = "_obmc_console._tcp"
	ep.Port = 2200

	dev := common.NewDevice()
	dev.NICs = []*common.NIC{{NICPorts: []*common.NICPort{{MacAddress: "aa:bb:cc:dd:ee:01"}}}}
	w, c := newWorker(t,
		&fakeBrowser{endpoints: []mdns.Endpoint{ep}},
		&fakeCollector{dev: &dev},
		credsSecret(map[string][]byte{"username": []byte("admin"), "password": []byte("pw")}),
	)
	w.RedfishPort = 443

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	if !eventually(t, func() bool {
		var machine bmcv1.Machine
		return c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine) == nil
	}) {
		t.Fatal("machine was not created")
	}
	var machine bmcv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine); err != nil {
		t.Fatal(err)
	}
	if machine.Spec.Connection.Port != 443 {
		t.Errorf("connection port = %d, want 443", machine.Spec.Connection.Port)
	}
	if opts := machine.Spec.Connection.ProviderOptions; opts == nil || opts.Redfish == nil || opts.Redfish.Port != 443 {
		t.Errorf("redfish provider port not overridden: %+v", opts)
	}
}

func TestWorkerGlobalDefaultCredentials(t *testing.T) {
	// With no secret and no service-specific default, the wildcard default
	// applies to any service type.
	dev := common.NewDevice()
	w, c := newWorker(t,
		&fakeBrowser{endpoints: []mdns.Endpoint{testEndpoint()}},
		&fakeCollector{dev: &dev, accept: &inventory.Credentials{Username: "admin", Password: "admin"}},
	)
	w.DefaultCredentials = map[string]inventory.Credentials{
		inventory.WildcardService: {Username: "admin", Password: "admin"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	if !eventually(t, func() bool {
		var machine bmcv1.Machine
		return c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine) == nil
	}) {
		t.Fatal("machine was not created with the global default credentials")
	}
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t-bmc-auth"}, &secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["username"]) != "admin" || string(secret.Data["password"]) != "admin" {
		t.Errorf("auth secret data = %v, want admin/admin", secret.Data)
	}
}

func TestWorkerPivotsToDefaultWhenSecretCredsFail(t *testing.T) {
	// The secret's credentials are tried first; when the BMC rejects them,
	// the worker pivots to the known defaults, and the per-machine auth
	// secret records the pair that actually worked.
	dev := common.NewDevice()
	w, c := newWorker(t,
		&fakeBrowser{endpoints: []mdns.Endpoint{testEndpoint()}},
		&fakeCollector{dev: &dev, accept: &inventory.Credentials{Username: "root", Password: "0penBmc"}},
		credsSecret(map[string][]byte{"username": []byte("wrong"), "password": []byte("wrong")}),
	)
	w.DefaultCredentials = map[string]inventory.Credentials{
		"_obmc_redfish._tcp":      {Username: "root", Password: "0penBmc"},
		inventory.WildcardService: {Username: "admin", Password: "admin"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	if !eventually(t, func() bool {
		var machine bmcv1.Machine
		return c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine) == nil
	}) {
		t.Fatal("machine was not created after pivoting to default credentials")
	}
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t-bmc-auth"}, &secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["username"]) != "root" || string(secret.Data["password"]) != "0penBmc" {
		t.Errorf("auth secret data = %v, want the working root/0penBmc pair", secret.Data)
	}
}

func TestWorkerAllCandidatesRejectedCreatesNothing(t *testing.T) {
	w, c := newWorker(t,
		&fakeBrowser{endpoints: []mdns.Endpoint{testEndpoint()}},
		&fakeCollector{accept: &inventory.Credentials{Username: "nobody", Password: "matches"}},
		credsSecret(map[string][]byte{"username": []byte("wrong"), "password": []byte("wrong")}),
	)
	w.DefaultCredentials = map[string]inventory.Credentials{
		inventory.WildcardService: {Username: "admin", Password: "admin"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	time.Sleep(300 * time.Millisecond)
	var machine bmcv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine); !apierrors.IsNotFound(err) {
		t.Fatalf("machine should not exist when every credential candidate fails, got err=%v", err)
	}
}

func TestWorkerMissingCredentialKeys(t *testing.T) {
	w, c := newWorker(t,
		&fakeBrowser{endpoints: []mdns.Endpoint{testEndpoint()}},
		&fakeCollector{},
		credsSecret(map[string][]byte{"password": []byte("pw")}), // no username
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	// Give the worker time to (incorrectly) create something.
	time.Sleep(300 * time.Millisecond)
	var machine bmcv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine); !apierrors.IsNotFound(err) {
		t.Fatalf("machine should not be created without valid credentials, got err=%v", err)
	}
}
