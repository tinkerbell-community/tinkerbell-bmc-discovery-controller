package controller

import (
	"context"
	"errors"
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
}

func (c *fakeCollector) Collect(context.Context, mdns.Endpoint, inventory.Credentials) (*common.Device, error) {
	return c.dev, c.err
}

func testEndpoint() mdns.Endpoint {
	return mdns.Endpoint{
		Instance: "X570D4I-2T",
		Service:  "_obmc_redfish._tcp",
		Hostname: "bmc.local.",
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
	return &Worker{
		Client:    c,
		Browser:   browser,
		Collector: collector,
		Syncer: &syncpkg.Syncer{
			Client:      c,
			Namespace:   "tink",
			InsecureTLS: true,
			Now:         time.Now,
		},
		CredentialsSecret: types.NamespacedName{Namespace: "tink", Name: "bmc-discovery-credentials"},
		ResyncInterval:    time.Hour,
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

func TestWorkerCollectFailureStillSyncsMachine(t *testing.T) {
	w, c := newWorker(t,
		&fakeBrowser{endpoints: []mdns.Endpoint{testEndpoint()}},
		&fakeCollector{err: errors.New("bmc unreachable")},
		credsSecret(map[string][]byte{"username": []byte("admin"), "password": []byte("pw")}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	if !eventually(t, func() bool {
		var machine bmcv1.Machine
		return c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &machine) == nil
	}) {
		t.Fatal("machine was not created")
	}
	var hw tinkv1.Hardware
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tink", Name: "x570d4i-2t"}, &hw); !apierrors.IsNotFound(err) {
		t.Fatalf("hardware should not exist without inventory, got err=%v", err)
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
