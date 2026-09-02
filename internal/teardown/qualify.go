package teardown

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// Qualifies reports whether a Machine is one this controller can service: a
// Talos node on Tinkerbell infrastructure. Machines of any other provider
// pair are ignored permanently — C3 must never park a deletion it cannot
// service.
func Qualifies(m *clusterv1.Machine) bool {
	return m.Spec.Bootstrap.ConfigRef.Kind == "TalosConfig" &&
		m.Spec.Bootstrap.ConfigRef.APIGroup == "bootstrap.cluster.x-k8s.io" &&
		m.Spec.InfrastructureRef.Kind == "TinkerbellMachine" &&
		m.Spec.InfrastructureRef.APIGroup == "infrastructure.cluster.x-k8s.io"
}

// HasHook reports whether the Machine carries C3's pre-terminate hook.
func HasHook(m *clusterv1.Machine) bool {
	_, ok := m.Annotations[HookAnnotation]
	return ok
}

// deletingReason returns the reason of the Deleting condition, or "" when
// the condition is absent (the machine controller has not processed the
// deletion yet).
func deletingReason(m *clusterv1.Machine) string {
	for _, c := range m.Status.Conditions {
		if c.Type == clusterv1.MachineDeletingCondition && c.Status == metav1.ConditionTrue {
			return c.Reason
		}
	}
	return ""
}

// preTerminateOrEarlier is the set of Deleting reasons at or before the
// pre-terminate wait; a late stamp is safe only in these phases.
var preTerminateOrEarlier = map[string]bool{
	clusterv1.MachineDeletingReason:                           true,
	clusterv1.MachineDeletingWaitingForPreDrainHookReason:     true,
	clusterv1.MachineDeletingDrainingNodeReason:               true,
	clusterv1.MachineDeletingWaitingForVolumeDetachReason:     true,
	clusterv1.MachineDeletingWaitingForPreTerminateHookReason: true,
}

// StampAllowed reports whether adding the hook annotation is safe. Before
// deletion it always is. After deletion it is safe only while the machine
// controller has not passed the pre-terminate gate: reconcileDelete
// re-evaluates the gate on every pass, so a late annotation on a Machine
// whose infra deletion (and power-off) has begun would park it mid-teardown.
func StampAllowed(m *clusterv1.Machine) bool {
	if m.DeletionTimestamp.IsZero() {
		return true
	}
	reason := deletingReason(m)
	return reason == "" || preTerminateOrEarlier[reason]
}

// AtPreTerminate reports whether the machine controller is parked at the
// pre-terminate hook wait for this Machine. Acting earlier (during drain)
// would kill the node while CAPI still needs its kubelet.
func AtPreTerminate(m *clusterv1.Machine) bool {
	return deletingReason(m) == clusterv1.MachineDeletingWaitingForPreTerminateHookReason
}

// HasOtherPreTerminateHooks reports whether any other controller still holds
// a pre-terminate hook on the Machine. C3 runs last (KCP kcp-cleanup
// discipline): its work stops the kubelet and etcd, which other hooks may
// still need.
func HasOtherPreTerminateHooks(m *clusterv1.Machine) bool {
	for key, value := range m.Annotations {
		// Keys carrying this controller's value are its own (current key,
		// or a pre-rename leftover swept at release) — counting a leftover
		// as foreign would hold the run-last gate against ourselves
		// forever.
		if key == HookAnnotation || value == HookValue {
			continue
		}
		if strings.HasPrefix(key, clusterv1.PreTerminateDeleteHookAnnotationPrefix) {
			return true
		}
	}
	return false
}

// legacyHookKeys returns hook keys under the CAPI pre-terminate prefix whose
// value names this controller but whose suffix is not the current one — a
// pre-rename leftover to be cleaned up alongside the current stamp.
func legacyHookKeys(m *clusterv1.Machine) []string {
	var keys []string
	for key, value := range m.Annotations {
		if key == HookAnnotation || value != HookValue {
			continue
		}
		if strings.HasPrefix(key, clusterv1.PreTerminateDeleteHookAnnotationPrefix) {
			keys = append(keys, key)
		}
	}
	return keys
}

// IsControlPlane reports whether the Machine is a control-plane machine.
func IsControlPlane(m *clusterv1.Machine) bool {
	_, ok := m.Labels[clusterv1.MachineControlPlaneLabel]
	return ok
}
