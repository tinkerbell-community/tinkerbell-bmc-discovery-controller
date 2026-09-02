package teardown

import (
	"context"
	"errors"
	"fmt"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// hookWaitRequeue is the fixed wait for the run-last and CP-serialization
// holds; both resolve via events C3 also watches, so this is a backstop.
const hookWaitRequeue = 15 * time.Second

// Reconciler implements the C3 pre-terminate state machine
// (docs/talos-machine-teardown.md): stamp the hook while healthy; at the
// pre-terminate wait run verify-then-act etcd removal (control plane), then
// a Talos Reset (wipe STATE+EPHEMERAL, halt), then release the hook.
type Reconciler struct {
	client.Client
	Recorder     events.EventRecorder
	TalosFactory ClientFactory
	Credentials  *CredentialCache

	// Phase deadlines, measured from the pre-terminate-observed-at
	// annotation; the reset clock includes the etcd phase.
	EtcdTimeout  time.Duration
	ResetTimeout time.Duration
	// Per-RPC timeouts.
	EtcdCallTimeout  time.Duration
	ResetCallTimeout time.Duration

	// Now is the clock (test seam); defaults to time.Now.
	Now func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile handles one Machine.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	machine := &clusterv1.Machine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !Qualifies(machine) && !HasHook(machine) {
		return ctrl.Result{}, nil
	}
	if machine.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileNormal(ctx, machine)
	}
	return r.reconcileDeleting(ctx, machine)
}

// reconcileNormal stamps the hook on healthy Machines and keeps the
// credential cache fresh.
func (r *Reconciler) reconcileNormal(ctx context.Context, machine *clusterv1.Machine) error {
	if err := r.ensureHook(ctx, machine); err != nil {
		return err
	}
	return r.ensureCredentials(ctx, machine)
}

// ensureCredentials refreshes the GC-proof talosconfig copy, surfacing
// writes as an event.
func (r *Reconciler) ensureCredentials(ctx context.Context, machine *clusterv1.Machine) error {
	wrote, err := r.Credentials.Ensure(ctx, clusterKeyFor(machine))
	if err != nil {
		return err
	}
	if wrote {
		r.event(machine, corev1.EventTypeNormal, EventCredentialsCached,
			"cached the cluster talosconfig for teardown during cluster deletion")
	}
	return nil
}

// ensureHook adds the pre-terminate hook annotation (and removes pre-rename
// leftovers) if allowed; idempotent.
func (r *Reconciler) ensureHook(ctx context.Context, machine *clusterv1.Machine) error {
	if HasHook(machine) || !StampAllowed(machine) {
		return nil
	}
	err := r.patchAnnotations(ctx, machine, func(ann map[string]string) bool {
		ann[HookAnnotation] = HookValue
		for _, key := range legacyHookKeys(machine) {
			delete(ann, key)
		}
		return true
	})
	if err != nil {
		return err
	}
	r.event(machine, corev1.EventTypeNormal, EventHookStamped,
		"Stamped pre-terminate teardown hook")
	return nil
}

// reconcileDeleting drives the pre-terminate state machine.
func (r *Reconciler) reconcileDeleting(ctx context.Context, machine *clusterv1.Machine) (ctrl.Result, error) {
	if !HasHook(machine) {
		// Teardown already concluded and released the hook: the Deleting
		// condition can still read WaitingForPreTerminateHook until the
		// machine controller's next pass, and re-stamping here would loop
		// (stamp → short-circuit → release → watch event → stamp ...).
		if _, done := machine.Annotations[ResetAnnotation]; done {
			return ctrl.Result{}, nil
		}
		// Late stamp, gated so a Machine whose power-off already began is
		// never parked mid-teardown (legacy path instead).
		if !StampAllowed(machine) {
			return ctrl.Result{}, nil
		}
		if err := r.ensureHook(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.ensureCredentials(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !AtPreTerminate(machine) {
		// Pre-drain, draining, or volume detach: the Machine watch
		// re-triggers on the condition transition. Never act early.
		return ctrl.Result{}, nil
	}
	if HasOtherPreTerminateHooks(machine) {
		// Run last: C3 stops the kubelet and etcd, which other hooks need.
		return ctrl.Result{RequeueAfter: hookWaitRequeue}, nil
	}

	observedAt, err := r.ensureObservedAt(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}

	clusterDeleting, err := r.clusterDeleting(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}

	if result, err := r.etcdPhase(ctx, machine, observedAt, clusterDeleting); err != nil || !result.IsZero() {
		return result, err
	}
	if err := r.resetPhase(ctx, machine, observedAt); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.release(ctx, machine)
}

// ensureObservedAt stamps the phase-entry timestamp once and returns it; all
// deadlines are measured from it so restarts do not reset the clock.
func (r *Reconciler) ensureObservedAt(ctx context.Context, machine *clusterv1.Machine) (time.Time, error) {
	if value, ok := machine.Annotations[ObservedAtAnnotation]; ok {
		observedAt, err := time.Parse(time.RFC3339, value)
		if err == nil {
			return observedAt, nil
		}
		// Unparseable (hand-edited): fall through and re-stamp.
	}
	observedAt := r.now().UTC().Truncate(time.Second)
	err := r.patchAnnotations(ctx, machine, func(ann map[string]string) bool {
		ann[ObservedAtAnnotation] = observedAt.Format(time.RFC3339)
		return true
	})
	return observedAt, err
}

// clusterDeleting reports cluster-delete mode: the owning Cluster is being
// deleted or is already gone.
func (r *Reconciler) clusterDeleting(ctx context.Context, machine *clusterv1.Machine) (bool, error) {
	cluster := &clusterv1.Cluster{}
	err := r.Get(ctx, clusterKeyFor(machine), cluster)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !cluster.DeletionTimestamp.IsZero(), nil
}

// clusterKeyFor derives the owning Cluster's key.
func clusterKeyFor(machine *clusterv1.Machine) client.ObjectKey {
	name := machine.Spec.ClusterName
	if name == "" {
		name = machine.Labels[clusterv1.ClusterNameLabel]
	}
	return client.ObjectKey{Namespace: machine.Namespace, Name: name}
}

// release removes the hook annotation — the final act, releasing the CAPI
// Machine controller to delete the infra machine (and CAPT to power off).
func (r *Reconciler) release(ctx context.Context, machine *clusterv1.Machine) error {
	if !HasHook(machine) {
		return nil
	}
	err := r.patchAnnotations(ctx, machine, func(ann map[string]string) bool {
		delete(ann, HookAnnotation)
		// Sweep any pre-rename leftovers in the same final patch so they
		// can never park the deletion.
		for _, key := range legacyHookKeys(machine) {
			delete(ann, key)
		}
		return true
	})
	if err != nil {
		return err
	}
	r.event(machine, corev1.EventTypeNormal, EventTeardownComplete,
		"Talos teardown complete; releasing machine deletion")
	machinesCompleted.Inc()
	return nil
}

// patchAnnotations applies an annotation mutation through the CAPI patch
// helper (optimistic-concurrency merge patch — the KCP/CACPPT precedent;
// disjoint keys cannot clobber sibling writers' annotations).
func (r *Reconciler) patchAnnotations(ctx context.Context, machine *clusterv1.Machine, mutate func(map[string]string) bool) error {
	helper, err := patch.NewHelper(machine, r.Client)
	if err != nil {
		return err
	}
	annotations := machine.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}
	if !mutate(annotations) {
		return nil
	}
	machine.SetAnnotations(annotations)
	return helper.Patch(ctx, machine)
}

// stampPhase records a phase conclusion annotation (its own patch, before
// the next phase begins — crash safety).
func (r *Reconciler) stampPhase(ctx context.Context, machine *clusterv1.Machine, key, value string) error {
	return r.patchAnnotations(ctx, machine, func(ann map[string]string) bool {
		if ann[key] == value {
			return false
		}
		ann[key] = value
		return true
	})
}

// --- etcd phase -----------------------------------------------------------

// etcdPhase runs the etcd half of the state machine. A zero result with nil
// error means the phase is concluded (annotation stamped) and the reset
// phase may proceed.
func (r *Reconciler) etcdPhase(ctx context.Context, machine *clusterv1.Machine, observedAt time.Time, clusterDeleting bool) (ctrl.Result, error) {
	if _, done := machine.Annotations[EtcdAnnotation]; done {
		return ctrl.Result{}, nil
	}
	if clusterDeleting {
		// Final-quorum decision: no etcd membership operations while the
		// whole cluster collapses — every peer is deleting, and the reset
		// wipe destroys member state anyway.
		if err := r.stampPhase(ctx, machine, EtcdAnnotation, EtcdSkippedClusterDelete); err != nil {
			return ctrl.Result{}, err
		}
		r.event(machine, corev1.EventTypeNormal, EventEtcdSkippedClusterDelete,
			"Cluster is being deleted; skipping etcd membership removal for the final quorum")
		etcdOutcomes.WithLabelValues(EtcdSkippedClusterDelete).Inc()
		return ctrl.Result{}, nil
	}
	if !IsControlPlane(machine) {
		return ctrl.Result{}, nil
	}
	if result, err := r.serializeControlPlane(ctx, machine); err != nil || !result.IsZero() {
		return result, err
	}
	return r.ensureEtcdRemoved(ctx, machine, observedAt)
}

// serializeControlPlane holds all but the oldest-deleting hooked CP Machine
// of a cluster: one etcd operation at a time, oldest deletionTimestamp
// first (KCP's deterministic pick).
func (r *Reconciler) serializeControlPlane(ctx context.Context, machine *clusterv1.Machine) (ctrl.Result, error) {
	peers, err := r.listControlPlaneMachines(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range peers {
		peer := &peers[i]
		if peer.Name == machine.Name || peer.DeletionTimestamp.IsZero() || !HasHook(peer) {
			continue
		}
		if peer.DeletionTimestamp.Time.Before(machine.DeletionTimestamp.Time) ||
			(peer.DeletionTimestamp.Time.Equal(machine.DeletionTimestamp.Time) && peer.Name < machine.Name) {
			return ctrl.Result{RequeueAfter: hookWaitRequeue}, nil
		}
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) listControlPlaneMachines(ctx context.Context, machine *clusterv1.Machine) ([]clusterv1.Machine, error) {
	var machines clusterv1.MachineList
	err := r.List(ctx, &machines, client.InNamespace(machine.Namespace), client.MatchingLabels{
		clusterv1.ClusterNameLabel: clusterKeyFor(machine).Name,
	}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	return machines.Items, err
}

// errTransient wraps failures that should retry with the controller's
// exponential backoff while the phase deadline has not expired.
var errTransient = errors.New("transient teardown failure")

// ensureEtcdRemoved is the verify-then-act etcd branch: membership is always
// re-checked against a live peer (CACPPT's etcd-leaving annotation is a hint
// only — it means "attempted", and failed leaves are swallowed by the
// scale.go leaveErr bug), a completed leave is detected as not-member, and
// every action is idempotent by construction.
func (r *Reconciler) ensureEtcdRemoved(ctx context.Context, machine *clusterv1.Machine, observedAt time.Time) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	deadline := observedAt.Add(r.EtcdTimeout)

	outcome, err := r.tryEtcdRemoval(ctx, machine)
	if err != nil {
		if r.now().After(deadline) {
			log.Info("etcd phase deadline expired; orphaning member", "error", err)
			r.event(machine, corev1.EventTypeWarning, EventEtcdMemberOrphaned,
				"etcd membership removal did not complete within %s (%v); proceeding — CACPPT auditEtcd covers the residual member", r.EtcdTimeout, err)
			outcome = EtcdOrphaned
		} else {
			return ctrl.Result{}, fmt.Errorf("%w: %w", errTransient, err)
		}
	}
	if err := r.stampPhase(ctx, machine, EtcdAnnotation, outcome); err != nil {
		return ctrl.Result{}, err
	}
	r.recordEtcdOutcome(machine, outcome)
	return ctrl.Result{}, nil
}

func (r *Reconciler) recordEtcdOutcome(machine *clusterv1.Machine, outcome string) {
	etcdOutcomes.WithLabelValues(outcome).Inc()
	switch outcome {
	case EtcdNotMember:
		// Common case after a successful CACPPT scale-down leave; no event.
	case EtcdLeft:
		r.event(machine, corev1.EventTypeNormal, EventEtcdMemberLeft,
			"etcd member left the cluster gracefully")
	case EtcdRemoved:
		r.event(machine, corev1.EventTypeNormal, EventEtcdMemberRemoved,
			"etcd member removed via a healthy peer")
	}
}

// tryEtcdRemoval performs one verify-then-act attempt. It returns the
// conclusive outcome, or an error when the attempt should be retried.
func (r *Reconciler) tryEtcdRemoval(ctx context.Context, machine *clusterv1.Machine) (string, error) {
	cfg, err := r.Credentials.Config(ctx, clusterKeyFor(machine))
	if err != nil {
		r.event(machine, corev1.EventTypeWarning, EventCredentialsMissing,
			"no talosconfig available (live or cached): %v", err)
		return "", fmt.Errorf("loading talosconfig: %w", err)
	}

	peerClient, memberResp, err := r.findHealthyPeer(ctx, machine, cfg)
	if err != nil {
		return "", err
	}
	defer peerClient.Close()

	member := findMember(memberResp, hostnameCandidates(machine))
	if member == nil {
		return EtcdNotMember, nil
	}

	if r.leaveViaVictim(ctx, machine, cfg) {
		return EtcdLeft, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, r.EtcdCallTimeout)
	defer cancel()
	if err := peerClient.EtcdRemoveMemberByID(callCtx, &machineapi.EtcdRemoveMemberByIDRequest{
		MemberId: member.GetId(),
	}); err != nil {
		return "", fmt.Errorf("removing etcd member %q via peer: %w", member.GetHostname(), err)
	}
	return EtcdRemoved, nil
}

// findHealthyPeer selects a healthy control-plane peer and returns a client
// dialed at it plus the member list it answered. Candidates: same cluster,
// control-plane, not the victim, not deleting, not mid-leave
// (etcd-leaving); NodeRef-holders first, then oldest. Unlike CACPPT's
// auditEtcd, C3 does not require a fully-settled fleet — one healthy peer
// suffices.
func (r *Reconciler) findHealthyPeer(ctx context.Context, machine *clusterv1.Machine, cfg *talosconfig.Config) (Client, *machineapi.EtcdMemberListResponse, error) {
	peers, err := r.listControlPlaneMachines(ctx, machine)
	if err != nil {
		return nil, nil, err
	}
	for _, peer := range orderPeers(peers, machine) {
		peerClient, resp, ok := r.probePeer(ctx, &peer, cfg)
		if ok {
			return peerClient, resp, nil
		}
	}
	return nil, nil, errors.New("no healthy etcd peer reachable")
}

// probePeer dials one candidate and checks etcd health + member list. Each
// RPC gets its own EtcdCallTimeout (per-RPC, per the flag's contract —
// budgets are not shared across a dialog).
func (r *Reconciler) probePeer(ctx context.Context, peer *clusterv1.Machine, cfg *talosconfig.Config) (Client, *machineapi.EtcdMemberListResponse, bool) {
	log := logf.FromContext(ctx)
	address, err := r.resolveAddress(ctx, peer)
	if err != nil || address == "" {
		// A transient failure resolving a candidate must stay diagnosable:
		// it can be the difference between a graceful leave and orphaning.
		log.V(1).Info("skipping peer candidate", "peer", peer.Name, "address", address, "error", err)
		return nil, nil, false
	}
	peerClient, err := r.TalosFactory(ctx, cfg, address)
	if err != nil {
		log.V(1).Info("skipping peer candidate: dial failed", "peer", peer.Name, "error", err)
		return nil, nil, false
	}
	svcs, err := r.callServiceInfo(ctx, peerClient)
	if err != nil || !etcdHealthy(svcs) {
		_ = peerClient.Close()
		return nil, nil, false
	}
	memberCtx, cancel := context.WithTimeout(ctx, r.EtcdCallTimeout)
	defer cancel()
	resp, err := peerClient.EtcdMemberList(memberCtx, &machineapi.EtcdMemberListRequest{})
	if err != nil {
		log.V(1).Info("peer answered ServiceInfo but not EtcdMemberList", "peer", peer.Name, "error", err)
		_ = peerClient.Close()
		return nil, nil, false
	}
	return peerClient, resp, true
}

// callServiceInfo wraps ServiceInfo("etcd") in its own per-RPC timeout.
func (r *Reconciler) callServiceInfo(ctx context.Context, c Client) ([]talosclient.ServiceInfo, error) {
	callCtx, cancel := context.WithTimeout(ctx, r.EtcdCallTimeout)
	defer cancel()
	return c.ServiceInfo(callCtx, "etcd")
}

// orderPeers filters and orders healthy-peer candidates.
func orderPeers(machines []clusterv1.Machine, victim *clusterv1.Machine) []clusterv1.Machine {
	var withNodeRef, withoutNodeRef []clusterv1.Machine
	for _, m := range machines {
		if m.Name == victim.Name || !m.DeletionTimestamp.IsZero() {
			continue
		}
		if _, leaving := m.Annotations[EtcdLeavingAnnotation]; leaving {
			continue
		}
		if m.Status.NodeRef.Name != "" {
			withNodeRef = append(withNodeRef, m)
		} else {
			withoutNodeRef = append(withoutNodeRef, m)
		}
	}
	sortByAge(withNodeRef)
	sortByAge(withoutNodeRef)
	return append(withNodeRef, withoutNodeRef...)
}

// leaveViaVictim attempts the graceful leave through the victim itself.
// Returns true only on confirmed success; any failure falls through to
// removal via the peer.
func (r *Reconciler) leaveViaVictim(ctx context.Context, machine *clusterv1.Machine, cfg *talosconfig.Config) bool {
	log := logf.FromContext(ctx)
	address, err := r.resolveAddress(ctx, machine)
	if err != nil || address == "" {
		return false
	}
	victimClient, err := r.TalosFactory(ctx, cfg, address)
	if err != nil {
		return false
	}
	defer victimClient.Close()

	svcs, err := r.callServiceInfo(ctx, victimClient)
	if err != nil || !etcdNotFinished(svcs) {
		return false
	}
	// The etcd-leaving hint means CACPPT already attempted a leave; skip
	// the leadership-forfeit nicety and go straight to the leave retry.
	if _, leaving := machine.Annotations[EtcdLeavingAnnotation]; !leaving {
		forfeitCtx, cancel := context.WithTimeout(ctx, r.EtcdCallTimeout)
		if _, err := victimClient.EtcdForfeitLeadership(forfeitCtx, &machineapi.EtcdForfeitLeadershipRequest{}); err != nil {
			log.V(1).Info("forfeiting etcd leadership failed; continuing", "error", err)
		}
		cancel()
	}
	leaveCtx, cancel := context.WithTimeout(ctx, r.EtcdCallTimeout)
	defer cancel()
	if err := victimClient.EtcdLeaveCluster(leaveCtx, &machineapi.EtcdLeaveClusterRequest{}); err != nil {
		log.Info("graceful etcd leave via victim failed; falling back to removal via peer", "error", err)
		return false
	}
	return true
}

// --- reset phase ----------------------------------------------------------

// resetPhase wipes STATE+EPHEMERAL and halts the node. Graceful is false
// because CAPI already drained the node (or deliberately skipped drain on
// cluster delete) and etcd membership was handled explicitly; a graceful
// reset would double-leave or hang on a node whose member was removed via a
// peer. Reboot is false so a wiped machine halts instead of racing CAPT's
// power-off into a netboot/auto-enrollment window.
func (r *Reconciler) resetPhase(ctx context.Context, machine *clusterv1.Machine, observedAt time.Time) error {
	if _, done := machine.Annotations[ResetAnnotation]; done {
		return nil
	}
	deadline := observedAt.Add(r.ResetTimeout)

	outcome, err := r.tryReset(ctx, machine)
	if err != nil {
		if !r.now().After(deadline) {
			return fmt.Errorf("%w: %w", errTransient, err)
		}
		r.event(machine, corev1.EventTypeWarning, EventResetSkipped,
			"Talos reset did not complete within %s (%v); proceeding — the next provisioning re-images the disk", r.ResetTimeout, err)
		outcome = ResetSkippedTimeout
	}
	// Stamped in its own patch before the hook is removed, so a crash after
	// the reset ack never re-resets a halted node on replay.
	if err := r.stampPhase(ctx, machine, ResetAnnotation, outcome); err != nil {
		return err
	}
	resetOutcomes.WithLabelValues(outcome).Inc()
	if outcome == ResetSkippedNoAddress {
		r.event(machine, corev1.EventTypeNormal, EventResetSkipped,
			"no address found on any source; nothing to wipe (node never booted)")
	}
	if outcome == ResetDone {
		r.event(machine, corev1.EventTypeNormal, EventResetSucceeded,
			"Talos reset acknowledged: STATE and EPHEMERAL wiped, node halting")
	}
	return nil
}

// tryReset performs one reset attempt; a conclusive outcome or a retriable
// error.
func (r *Reconciler) tryReset(ctx context.Context, machine *clusterv1.Machine) (string, error) {
	address, err := r.resolveAddress(ctx, machine)
	if err != nil {
		return "", err
	}
	if address == "" {
		return ResetSkippedNoAddress, nil
	}
	cfg, err := r.Credentials.Config(ctx, clusterKeyFor(machine))
	if err != nil {
		r.event(machine, corev1.EventTypeWarning, EventCredentialsMissing,
			"no talosconfig available (live or cached): %v", err)
		return "", fmt.Errorf("loading talosconfig: %w", err)
	}
	victimClient, err := r.TalosFactory(ctx, cfg, address)
	if err != nil {
		return "", err
	}
	defer victimClient.Close()

	callCtx, cancel := context.WithTimeout(ctx, r.ResetCallTimeout)
	defer cancel()
	started := r.now()
	err = victimClient.ResetGeneric(callCtx, &machineapi.ResetRequest{
		Graceful: false,
		Reboot:   false,
		SystemPartitionsToWipe: []*machineapi.ResetPartitionSpec{
			{Label: "STATE", Wipe: true},
			{Label: "EPHEMERAL", Wipe: true},
		},
	})
	if err != nil {
		return "", fmt.Errorf("resetting node at %s: %w", address, err)
	}
	resetDuration.Observe(r.now().Sub(started).Seconds())
	return ResetDone, nil
}

// event emits a Machine-scoped event through the structured events API; the
// action is the reason (each reason names one teardown step).
func (r *Reconciler) event(machine *clusterv1.Machine, eventtype, reason, note string, args ...any) {
	r.Recorder.Eventf(machine, nil, eventtype, reason, reason, note, args...)
}
