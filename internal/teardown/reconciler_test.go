package teardown

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- fake Talos node fleet -------------------------------------------------

// talosFleet scripts per-endpoint Talos node behavior and records calls.
type talosFleet struct {
	mu sync.Mutex
	// etcdState maps endpoint -> service state ("Running", "Finished");
	// endpoints absent from the map are unreachable.
	etcdState map[string]string
	members   []*machineapi.EtcdMember
	leaveErr  error
	removeErr error
	resetErr  error
	calls     []string
}

func (f *talosFleet) record(endpoint, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, endpoint+":"+call)
}

func (f *talosFleet) callsMade() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *talosFleet) called(want string) bool {
	return slices.Contains(f.callsMade(), want)
}

func (f *talosFleet) factory(_ context.Context, _ *talosconfig.Config, endpoint string) (Client, error) {
	return &fakeNode{fleet: f, endpoint: endpoint}, nil
}

type fakeNode struct {
	fleet    *talosFleet
	endpoint string
}

// unreachable models a dead node: every RPC fails, not just ServiceInfo.
func (n *fakeNode) unreachable() error {
	if _, ok := n.fleet.etcdState[n.endpoint]; !ok {
		return fmt.Errorf("endpoint %s unreachable", n.endpoint)
	}
	return nil
}

func (n *fakeNode) ServiceInfo(_ context.Context, _ string) ([]talosclient.ServiceInfo, error) {
	n.fleet.record(n.endpoint, "ServiceInfo")
	if err := n.unreachable(); err != nil {
		return nil, err
	}
	state := n.fleet.etcdState[n.endpoint]
	return []talosclient.ServiceInfo{{Service: &machineapi.ServiceInfo{
		Id: "etcd", State: state, Health: &machineapi.ServiceHealth{Healthy: state == "Running"},
	}}}, nil
}

func (n *fakeNode) EtcdMemberList(_ context.Context, _ *machineapi.EtcdMemberListRequest) (*machineapi.EtcdMemberListResponse, error) {
	n.fleet.record(n.endpoint, "EtcdMemberList")
	return &machineapi.EtcdMemberListResponse{Messages: []*machineapi.EtcdMembers{
		{Members: n.fleet.members},
	}}, nil
}

func (n *fakeNode) EtcdForfeitLeadership(_ context.Context, _ *machineapi.EtcdForfeitLeadershipRequest) (*machineapi.EtcdForfeitLeadershipResponse, error) {
	n.fleet.record(n.endpoint, "EtcdForfeitLeadership")
	return &machineapi.EtcdForfeitLeadershipResponse{}, nil
}

func (n *fakeNode) EtcdLeaveCluster(_ context.Context, _ *machineapi.EtcdLeaveClusterRequest) error {
	n.fleet.record(n.endpoint, "EtcdLeaveCluster")
	return n.fleet.leaveErr
}

func (n *fakeNode) EtcdRemoveMemberByID(_ context.Context, _ *machineapi.EtcdRemoveMemberByIDRequest) error {
	n.fleet.record(n.endpoint, "EtcdRemoveMemberByID")
	return n.fleet.removeErr
}

func (n *fakeNode) ResetGeneric(_ context.Context, req *machineapi.ResetRequest) error {
	n.fleet.record(n.endpoint, "ResetGeneric")
	if err := n.unreachable(); err != nil {
		return err
	}
	if req.Graceful || req.Reboot {
		return errors.New("test contract: reset must be graceful=false reboot=false")
	}
	return n.fleet.resetErr
}

func (n *fakeNode) Close() error { return nil }

// --- fixtures --------------------------------------------------------------

const (
	testNamespace = "tinkerbell"
	ownNamespace  = "teardown-system"
	clusterName   = "talos"
)

// minimalTalosconfig parses via talosconfig.FromBytes; the fake factory
// never dials with it.
const minimalTalosconfig = `context: test
contexts:
  test:
    endpoints:
      - 10.0.0.1
`

func teardownScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func testCluster() *clusterv1.Cluster {
	return &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: testNamespace}}
}

func talosconfigSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-talosconfig", Namespace: testNamespace},
		Data:       map[string][]byte{"talosconfig": []byte(minimalTalosconfig)},
	}
}

// machineFixture builds a qualifying Machine named name with address addr.
func machineFixture(name, addr string, controlPlane bool) *clusterv1.Machine {
	m := talosMachine()
	m.Name = name
	m.Spec.InfrastructureRef.Name = name + "-infra"
	m.Labels = map[string]string{clusterv1.ClusterNameLabel: clusterName}
	if controlPlane {
		m.Labels[clusterv1.MachineControlPlaneLabel] = ""
	}
	m.Status.NodeRef = clusterv1.MachineNodeReference{Name: name}
	if addr != "" {
		m.Status.Addresses = clusterv1.MachineAddresses{
			{Type: clusterv1.MachineInternalIP, Address: addr},
		}
	}
	return m
}

// deletingAtPreTerminate marks a fixture as deleting, hooked, and parked at
// the pre-terminate wait.
func deletingAtPreTerminate(m *clusterv1.Machine) *clusterv1.Machine {
	now := metav1.Now()
	m.DeletionTimestamp = &now
	m.Finalizers = []string{"test.finalizer"}
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[HookAnnotation] = HookValue
	withDeleting(m, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)
	return m
}

type testHarness struct {
	client client.Client
	fleet  *talosFleet
	r      *Reconciler
	now    time.Time
}

func newHarness(t *testing.T, fleet *talosFleet, objs ...client.Object) *testHarness {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(teardownScheme(t)).WithObjects(objs...).Build()
	h := &testHarness{
		client: c,
		fleet:  fleet,
		now:    time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
	h.r = &Reconciler{
		Client:           c,
		Recorder:         events.NewFakeRecorder(64),
		TalosFactory:     fleet.factory,
		Credentials:      &CredentialCache{Client: c, Namespace: ownNamespace},
		EtcdTimeout:      2 * time.Minute,
		ResetTimeout:     5 * time.Minute,
		EtcdCallTimeout:  10 * time.Second,
		ResetCallTimeout: 30 * time.Second,
		Now:              func() time.Time { return h.now },
	}
	return h
}

func (h *testHarness) reconcile(t *testing.T, name string) (ctrl.Result, error) {
	t.Helper()
	return h.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: name},
	})
}

// reconcileUntilSettled drives reconciles until no transient error and no
// requeue remain (bounded), mimicking the workqueue.
func (h *testHarness) reconcileUntilSettled(t *testing.T, name string) {
	t.Helper()
	for range 10 {
		result, err := h.reconcile(t, name)
		if err == nil && result.IsZero() {
			return
		}
	}
	t.Fatal("reconcile did not settle within 10 iterations")
}

func (h *testHarness) getMachine(t *testing.T, name string) *clusterv1.Machine {
	t.Helper()
	m := &clusterv1.Machine{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// --- stamping --------------------------------------------------------------

func TestStampsHookOnHealthyMachine(t *testing.T) {
	h := newHarness(t, &talosFleet{}, machineFixture("cp-1", "10.0.0.1", true), testCluster(), talosconfigSecret())

	if _, err := h.reconcile(t, "cp-1"); err != nil {
		t.Fatal(err)
	}

	m := h.getMachine(t, "cp-1")
	if m.Annotations[HookAnnotation] != HookValue {
		t.Errorf("hook not stamped: %v", m.Annotations)
	}

	// Credential cache is populated at stamp time (cluster-delete GC race).
	var cached corev1.Secret
	if err := h.client.Get(context.Background(),
		client.ObjectKey{Namespace: ownNamespace, Name: "talosconfig-tinkerbell-talos"}, &cached); err != nil {
		t.Errorf("credential cache not populated: %v", err)
	}

	// Idempotent: a second reconcile changes nothing.
	before := m.ResourceVersion
	if _, err := h.reconcile(t, "cp-1"); err != nil {
		t.Fatal(err)
	}
	if after := h.getMachine(t, "cp-1").ResourceVersion; after != before {
		t.Errorf("second reconcile wrote (rv %s -> %s); stamping is not idempotent", before, after)
	}
}

func TestIgnoresForeignMachines(t *testing.T) {
	m := machineFixture("other-1", "10.0.0.9", false)
	m.Spec.Bootstrap.ConfigRef.Kind = "KubeadmConfig"
	h := newHarness(t, &talosFleet{}, m)

	if _, err := h.reconcile(t, "other-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.getMachine(t, "other-1").Annotations[HookAnnotation]; ok {
		t.Error("foreign machine must never be annotated")
	}
}

func TestNoLateStampPastPreTerminate(t *testing.T) {
	m := machineFixture("cp-1", "10.0.0.1", true)
	now := metav1.Now()
	m.DeletionTimestamp = &now
	m.Finalizers = []string{"test.finalizer"}
	withDeleting(m, clusterv1.MachineDeletingWaitingForInfrastructureDeletionReason)
	h := newHarness(t, &talosFleet{}, m, testCluster(), talosconfigSecret())

	if _, err := h.reconcile(t, "cp-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.getMachine(t, "cp-1").Annotations[HookAnnotation]; ok {
		t.Error("late stamp past the pre-terminate gate would park a machine mid-power-off")
	}
}

// --- deletion flow ---------------------------------------------------------

func TestWorkerTeardown(t *testing.T) {
	fleet := &talosFleet{etcdState: map[string]string{"10.0.0.5": "Running"}}
	worker := deletingAtPreTerminate(machineFixture("worker-1", "10.0.0.5", false))
	h := newHarness(t, fleet, worker, testCluster(), talosconfigSecret())

	h.reconcileUntilSettled(t, "worker-1")

	m := h.getMachine(t, "worker-1")
	if m.Annotations[ResetAnnotation] != ResetDone {
		t.Errorf("reset = %q, want %q", m.Annotations[ResetAnnotation], ResetDone)
	}
	if _, ok := m.Annotations[EtcdAnnotation]; ok {
		t.Errorf("worker must have no etcd phase, got %q", m.Annotations[EtcdAnnotation])
	}
	if _, ok := m.Annotations[HookAnnotation]; ok {
		t.Error("hook not released after teardown")
	}
	if !fleet.called("10.0.0.5:ResetGeneric") {
		t.Errorf("reset not issued to the worker: %v", fleet.callsMade())
	}
	for _, call := range fleet.callsMade() {
		if call == "10.0.0.5:EtcdLeaveCluster" || call == "10.0.0.5:EtcdRemoveMemberByID" {
			t.Errorf("worker teardown made an etcd call: %v", fleet.callsMade())
		}
	}
}

func TestControlPlaneGracefulLeave(t *testing.T) {
	fleet := &talosFleet{
		etcdState: map[string]string{"10.0.0.1": "Running", "10.0.0.2": "Running"},
		members: []*machineapi.EtcdMember{
			{Id: 1, Hostname: "cp-1"},
			{Id: 2, Hostname: "cp-2"},
		},
	}
	victim := deletingAtPreTerminate(machineFixture("cp-1", "10.0.0.1", true))
	peer := machineFixture("cp-2", "10.0.0.2", true)
	h := newHarness(t, fleet, victim, peer, testCluster(), talosconfigSecret())

	h.reconcileUntilSettled(t, "cp-1")

	m := h.getMachine(t, "cp-1")
	if m.Annotations[EtcdAnnotation] != EtcdLeft {
		t.Errorf("etcd = %q, want %q (calls: %v)", m.Annotations[EtcdAnnotation], EtcdLeft, fleet.callsMade())
	}
	if m.Annotations[ResetAnnotation] != ResetDone {
		t.Errorf("reset = %q, want %q", m.Annotations[ResetAnnotation], ResetDone)
	}
	if _, ok := m.Annotations[HookAnnotation]; ok {
		t.Error("hook not released")
	}
	// Membership verified via the peer, leave via the victim, with the
	// leadership-forfeit nicety (no etcd-leaving hint present).
	for _, want := range []string{"10.0.0.2:EtcdMemberList", "10.0.0.1:EtcdForfeitLeadership", "10.0.0.1:EtcdLeaveCluster", "10.0.0.1:ResetGeneric"} {
		if !fleet.called(want) {
			t.Errorf("missing call %s in %v", want, fleet.callsMade())
		}
	}
}

func TestControlPlaneLeavingHintSkipsForfeit(t *testing.T) {
	fleet := &talosFleet{
		etcdState: map[string]string{"10.0.0.1": "Running", "10.0.0.2": "Running"},
		members:   []*machineapi.EtcdMember{{Id: 1, Hostname: "cp-1"}},
	}
	victim := deletingAtPreTerminate(machineFixture("cp-1", "10.0.0.1", true))
	victim.Annotations[EtcdLeavingAnnotation] = "true"
	peer := machineFixture("cp-2", "10.0.0.2", true)
	h := newHarness(t, fleet, victim, peer, testCluster(), talosconfigSecret())

	h.reconcileUntilSettled(t, "cp-1")

	// The hint is NOT trusted as proof (CACPPT stamps it before the leave,
	// and failed leaves are swallowed): membership is verified and the
	// leave still runs — only the forfeit nicety is skipped.
	m := h.getMachine(t, "cp-1")
	if m.Annotations[EtcdAnnotation] != EtcdLeft {
		t.Errorf("etcd = %q, want %q", m.Annotations[EtcdAnnotation], EtcdLeft)
	}
	if !fleet.called("10.0.0.1:EtcdLeaveCluster") {
		t.Errorf("leave must still run despite the hint: %v", fleet.callsMade())
	}
	if fleet.called("10.0.0.1:EtcdForfeitLeadership") {
		t.Errorf("forfeit must be skipped with the hint present: %v", fleet.callsMade())
	}
}

func TestControlPlaneNotMemberSkips(t *testing.T) {
	fleet := &talosFleet{
		etcdState: map[string]string{"10.0.0.1": "Running", "10.0.0.2": "Running"},
		members:   []*machineapi.EtcdMember{{Id: 2, Hostname: "cp-2"}}, // victim already left
	}
	victim := deletingAtPreTerminate(machineFixture("cp-1", "10.0.0.1", true))
	peer := machineFixture("cp-2", "10.0.0.2", true)
	h := newHarness(t, fleet, victim, peer, testCluster(), talosconfigSecret())

	h.reconcileUntilSettled(t, "cp-1")

	m := h.getMachine(t, "cp-1")
	if m.Annotations[EtcdAnnotation] != EtcdNotMember {
		t.Errorf("etcd = %q, want %q", m.Annotations[EtcdAnnotation], EtcdNotMember)
	}
	for _, call := range fleet.callsMade() {
		if call == "10.0.0.1:EtcdLeaveCluster" || call == "10.0.0.2:EtcdRemoveMemberByID" {
			t.Errorf("no leave/remove for a non-member (double-leave safety): %v", fleet.callsMade())
		}
	}
}

func TestControlPlaneDeadVictimRemovedViaPeer(t *testing.T) {
	fleet := &talosFleet{
		// Victim endpoint absent -> unreachable.
		etcdState: map[string]string{"10.0.0.2": "Running"},
		members:   []*machineapi.EtcdMember{{Id: 1, Hostname: "cp-1"}, {Id: 2, Hostname: "cp-2"}},
	}
	victim := deletingAtPreTerminate(machineFixture("cp-1", "10.0.0.1", true))
	peer := machineFixture("cp-2", "10.0.0.2", true)
	h := newHarness(t, fleet, victim, peer, testCluster(), talosconfigSecret())

	// The reset to the dead victim keeps failing; advance the clock past
	// the reset deadline so the teardown concludes on the timeout path.
	for range 5 {
		_, _ = h.reconcile(t, "cp-1")
	}
	h.now = h.now.Add(6 * time.Minute)
	h.reconcileUntilSettled(t, "cp-1")

	m := h.getMachine(t, "cp-1")
	if m.Annotations[EtcdAnnotation] != EtcdRemoved {
		t.Errorf("etcd = %q, want %q (calls: %v)", m.Annotations[EtcdAnnotation], EtcdRemoved, fleet.callsMade())
	}
	if !fleet.called("10.0.0.2:EtcdRemoveMemberByID") {
		t.Errorf("dead victim must be removed via the peer: %v", fleet.callsMade())
	}
	if m.Annotations[ResetAnnotation] != ResetSkippedTimeout {
		t.Errorf("reset = %q, want %q", m.Annotations[ResetAnnotation], ResetSkippedTimeout)
	}
	if _, ok := m.Annotations[HookAnnotation]; ok {
		t.Error("hook must be released after the timeout path (fail open)")
	}
}

func TestNoHealthyPeerOrphansAfterDeadline(t *testing.T) {
	fleet := &talosFleet{etcdState: map[string]string{"10.0.0.1": "Running"}}
	victim := deletingAtPreTerminate(machineFixture("cp-1", "10.0.0.1", true))
	h := newHarness(t, fleet, victim, testCluster(), talosconfigSecret())

	if _, err := h.reconcile(t, "cp-1"); err == nil {
		t.Fatal("expected a transient error while within the etcd deadline")
	}
	m := h.getMachine(t, "cp-1")
	if _, ok := m.Annotations[EtcdAnnotation]; ok {
		t.Fatalf("etcd concluded prematurely: %q", m.Annotations[EtcdAnnotation])
	}

	h.now = h.now.Add(3 * time.Minute)
	h.reconcileUntilSettled(t, "cp-1")

	m = h.getMachine(t, "cp-1")
	if m.Annotations[EtcdAnnotation] != EtcdOrphaned {
		t.Errorf("etcd = %q, want %q", m.Annotations[EtcdAnnotation], EtcdOrphaned)
	}
	if _, ok := m.Annotations[HookAnnotation]; ok {
		t.Error("hook must be released after orphaning (fail open)")
	}
}

func TestClusterDeleteModeSkipsEtcd(t *testing.T) {
	fleet := &talosFleet{
		etcdState: map[string]string{"10.0.0.1": "Running", "10.0.0.2": "Running"},
		members:   []*machineapi.EtcdMember{{Id: 1, Hostname: "cp-1"}},
	}
	victim := deletingAtPreTerminate(machineFixture("cp-1", "10.0.0.1", true))
	peer := machineFixture("cp-2", "10.0.0.2", true)
	cluster := testCluster()
	now := metav1.Now()
	cluster.DeletionTimestamp = &now
	cluster.Finalizers = []string{"test.finalizer"}
	h := newHarness(t, fleet, victim, peer, cluster, talosconfigSecret())

	h.reconcileUntilSettled(t, "cp-1")

	m := h.getMachine(t, "cp-1")
	if m.Annotations[EtcdAnnotation] != EtcdSkippedClusterDelete {
		t.Errorf("etcd = %q, want %q", m.Annotations[EtcdAnnotation], EtcdSkippedClusterDelete)
	}
	for _, call := range fleet.callsMade() {
		if call == "10.0.0.2:EtcdMemberList" || call == "10.0.0.1:EtcdLeaveCluster" {
			t.Errorf("no etcd membership operations during cluster delete: %v", fleet.callsMade())
		}
	}
	if m.Annotations[ResetAnnotation] != ResetDone {
		t.Errorf("reset still runs during cluster delete, got %q", m.Annotations[ResetAnnotation])
	}
}

func TestClusterDeleteUsesCachedCredentials(t *testing.T) {
	fleet := &talosFleet{etcdState: map[string]string{"10.0.0.5": "Running"}}
	worker := deletingAtPreTerminate(machineFixture("worker-1", "10.0.0.5", false))
	cached := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talosconfig-tinkerbell-talos",
			Namespace: ownNamespace,
			Labels:    map[string]string{ClusterNameLabel: clusterName, ClusterNamespaceLabel: testNamespace},
		},
		Data: map[string][]byte{"talosconfig": []byte(minimalTalosconfig)},
	}
	// No Cluster, no live secret: only the GC-proof cached copy remains.
	h := newHarness(t, fleet, worker, cached)

	h.reconcileUntilSettled(t, "worker-1")

	m := h.getMachine(t, "worker-1")
	if m.Annotations[ResetAnnotation] != ResetDone {
		t.Errorf("reset = %q, want %q (cached credentials should carry the teardown)", m.Annotations[ResetAnnotation], ResetDone)
	}
}

func TestNoAddressSkipsReset(t *testing.T) {
	fleet := &talosFleet{}
	worker := deletingAtPreTerminate(machineFixture("worker-1", "", false))
	h := newHarness(t, fleet, worker, testCluster(), talosconfigSecret())

	h.reconcileUntilSettled(t, "worker-1")

	m := h.getMachine(t, "worker-1")
	if m.Annotations[ResetAnnotation] != ResetSkippedNoAddress {
		t.Errorf("reset = %q, want %q", m.Annotations[ResetAnnotation], ResetSkippedNoAddress)
	}
	if len(fleet.callsMade()) != 0 {
		t.Errorf("no Talos calls expected without an address: %v", fleet.callsMade())
	}
}

func TestRunLastHoldsForOtherHooks(t *testing.T) {
	fleet := &talosFleet{}
	victim := deletingAtPreTerminate(machineFixture("cp-1", "10.0.0.1", true))
	victim.Annotations[clusterv1.PreTerminateDeleteHookAnnotationPrefix+"/other"] = "someone"
	h := newHarness(t, fleet, victim, testCluster(), talosconfigSecret())

	result, err := h.reconcile(t, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != hookWaitRequeue {
		t.Errorf("RequeueAfter = %v, want %v (run-last discipline)", result.RequeueAfter, hookWaitRequeue)
	}
	if len(fleet.callsMade()) != 0 {
		t.Errorf("no Talos calls while another hook holds: %v", fleet.callsMade())
	}
}

func TestControlPlaneSerialization(t *testing.T) {
	fleet := &talosFleet{etcdState: map[string]string{"10.0.0.1": "Running", "10.0.0.2": "Running", "10.0.0.3": "Running"}}
	older := deletingAtPreTerminate(machineFixture("cp-1", "10.0.0.1", true))
	past := metav1.NewTime(time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC))
	older.DeletionTimestamp = &past
	newer := deletingAtPreTerminate(machineFixture("cp-2", "10.0.0.2", true))
	peer := machineFixture("cp-3", "10.0.0.3", true)
	h := newHarness(t, fleet, older, newer, peer, testCluster(), talosconfigSecret())

	result, err := h.reconcile(t, "cp-2")
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != hookWaitRequeue {
		t.Errorf("newer CP machine must wait for the older one: RequeueAfter = %v", result.RequeueAfter)
	}
}

func TestNotAtPreTerminateDoesNothing(t *testing.T) {
	fleet := &talosFleet{}
	m := machineFixture("cp-1", "10.0.0.1", true)
	now := metav1.Now()
	m.DeletionTimestamp = &now
	m.Finalizers = []string{"test.finalizer"}
	m.Annotations = map[string]string{HookAnnotation: HookValue}
	withDeleting(m, clusterv1.MachineDeletingDrainingNodeReason)
	h := newHarness(t, fleet, m, testCluster(), talosconfigSecret())

	result, err := h.reconcile(t, "cp-1")
	if err != nil || !result.IsZero() {
		t.Fatalf("result=%v err=%v, want inert", result, err)
	}
	if len(fleet.callsMade()) != 0 {
		t.Errorf("acting during drain would kill a node CAPI still needs: %v", fleet.callsMade())
	}
}

// TestCrashSafetyReplay verifies phase conclusions persist: after the etcd
// and reset phases conclude, replaying reconciles never re-dials Talos.
func TestCrashSafetyReplay(t *testing.T) {
	fleet := &talosFleet{
		etcdState: map[string]string{"10.0.0.1": "Running", "10.0.0.2": "Running"},
		members:   []*machineapi.EtcdMember{{Id: 1, Hostname: "cp-1"}},
	}
	victim := deletingAtPreTerminate(machineFixture("cp-1", "10.0.0.1", true))
	peer := machineFixture("cp-2", "10.0.0.2", true)
	h := newHarness(t, fleet, victim, peer, testCluster(), talosconfigSecret())

	h.reconcileUntilSettled(t, "cp-1")
	calls := len(fleet.callsMade())

	// Simulate a replay after the hook was already released: re-add the
	// hook as a crashed-controller would see it (phases already stamped).
	m := h.getMachine(t, "cp-1")
	m.Annotations[HookAnnotation] = HookValue
	if err := h.client.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	h.reconcileUntilSettled(t, "cp-1")

	if got := len(fleet.callsMade()); got != calls {
		t.Errorf("replay made %d extra Talos calls; phase stamps must short-circuit", got-calls)
	}
	if _, ok := h.getMachine(t, "cp-1").Annotations[HookAnnotation]; ok {
		t.Error("replay must release the hook again")
	}
}

// TestNoRestampAfterRelease guards against the post-release hot loop: after
// teardown concludes and the hook is removed, the Deleting condition still
// reads WaitingForPreTerminateHook until the machine controller's next pass
// — a reconcile in that window must not re-stamp the hook (which would
// re-park the deletion and loop stamp/release forever).
func TestNoRestampAfterRelease(t *testing.T) {
	fleet := &talosFleet{etcdState: map[string]string{"10.0.0.5": "Running"}}
	worker := deletingAtPreTerminate(machineFixture("worker-1", "10.0.0.5", false))
	h := newHarness(t, fleet, worker, testCluster(), talosconfigSecret())

	h.reconcileUntilSettled(t, "worker-1")
	released := h.getMachine(t, "worker-1")
	if _, ok := released.Annotations[HookAnnotation]; ok {
		t.Fatal("teardown did not release the hook")
	}

	// The watch event from the release patch triggers another reconcile
	// while the Deleting condition is unchanged.
	if _, err := h.reconcile(t, "worker-1"); err != nil {
		t.Fatal(err)
	}
	m := h.getMachine(t, "worker-1")
	if _, ok := m.Annotations[HookAnnotation]; ok {
		t.Error("hook re-stamped after release: stamp/release loop")
	}
	if m.ResourceVersion != released.ResourceVersion {
		t.Errorf("post-release reconcile wrote (rv %s -> %s); must be inert", released.ResourceVersion, m.ResourceVersion)
	}
}
