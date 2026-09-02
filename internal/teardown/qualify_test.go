package teardown

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

func talosMachine() *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-1", Namespace: "tinkerbell"},
		Spec: clusterv1.MachineSpec{
			ClusterName: "talos",
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "TalosConfig", Name: "cp-1",
				},
			},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "TinkerbellMachine", Name: "cp-1-infra",
			},
		},
	}
}

func withDeleting(m *clusterv1.Machine, reason string) *clusterv1.Machine {
	m.Status.Conditions = append(m.Status.Conditions, metav1.Condition{
		Type: clusterv1.MachineDeletingCondition, Status: metav1.ConditionTrue, Reason: reason,
	})
	return m
}

func TestQualifies(t *testing.T) {
	if !Qualifies(talosMachine()) {
		t.Error("Talos+Tinkerbell machine should qualify")
	}
	kubeadm := talosMachine()
	kubeadm.Spec.Bootstrap.ConfigRef.Kind = "KubeadmConfig"
	if Qualifies(kubeadm) {
		t.Error("KubeadmConfig machine must not qualify")
	}
	otherInfra := talosMachine()
	otherInfra.Spec.InfrastructureRef.Kind = "DockerMachine"
	if Qualifies(otherInfra) {
		t.Error("non-Tinkerbell machine must not qualify")
	}
}

func TestStampAllowed(t *testing.T) {
	now := metav1.Now()

	if !StampAllowed(talosMachine()) {
		t.Error("healthy machine should allow stamping")
	}

	deleting := talosMachine()
	deleting.DeletionTimestamp = &now
	if !StampAllowed(deleting) {
		t.Error("deleting machine with no Deleting condition should allow stamping")
	}

	for reason, want := range map[string]bool{
		clusterv1.MachineDeletingWaitingForPreDrainHookReason:           true,
		clusterv1.MachineDeletingDrainingNodeReason:                     true,
		clusterv1.MachineDeletingWaitingForVolumeDetachReason:           true,
		clusterv1.MachineDeletingWaitingForPreTerminateHookReason:       true,
		clusterv1.MachineDeletingWaitingForInfrastructureDeletionReason: false,
		clusterv1.MachineDeletingWaitingForBootstrapDeletionReason:      false,
		clusterv1.MachineDeletingDeletingNodeReason:                     false,
	} {
		m := talosMachine()
		m.DeletionTimestamp = &now
		withDeleting(m, reason)
		if got := StampAllowed(m); got != want {
			t.Errorf("StampAllowed(reason=%s) = %v, want %v", reason, got, want)
		}
	}
}

func TestAtPreTerminate(t *testing.T) {
	if AtPreTerminate(talosMachine()) {
		t.Error("machine without Deleting condition is not at pre-terminate")
	}
	if AtPreTerminate(withDeleting(talosMachine(), clusterv1.MachineDeletingDrainingNodeReason)) {
		t.Error("draining machine is not at pre-terminate")
	}
	if !AtPreTerminate(withDeleting(talosMachine(), clusterv1.MachineDeletingWaitingForPreTerminateHookReason)) {
		t.Error("WaitingForPreTerminateHook is the pre-terminate phase")
	}
}

func TestHasOtherPreTerminateHooks(t *testing.T) {
	m := talosMachine()
	m.Annotations = map[string]string{HookAnnotation: HookValue}
	if HasOtherPreTerminateHooks(m) {
		t.Error("own hook alone is not an other hook")
	}
	m.Annotations[clusterv1.PreTerminateDeleteHookAnnotationPrefix+"/kcp-cleanup"] = "kcp"
	if !HasOtherPreTerminateHooks(m) {
		t.Error("foreign pre-terminate hook must be detected")
	}
}

func TestLegacyHookKeys(t *testing.T) {
	m := talosMachine()
	m.Annotations = map[string]string{
		HookAnnotation: HookValue,
		clusterv1.PreTerminateDeleteHookAnnotationPrefix + "/old-name": HookValue,
		clusterv1.PreTerminateDeleteHookAnnotationPrefix + "/kcp":      "kcp",
	}
	keys := legacyHookKeys(m)
	if len(keys) != 1 || keys[0] != clusterv1.PreTerminateDeleteHookAnnotationPrefix+"/old-name" {
		t.Errorf("legacyHookKeys = %v, want only the pre-rename leftover", keys)
	}
}

func TestHostnameCandidates(t *testing.T) {
	m := talosMachine()
	m.Status.NodeRef = clusterv1.MachineNodeReference{Name: "node-a"}
	m.Status.Addresses = clusterv1.MachineAddresses{
		{Type: clusterv1.MachineHostName, Address: "host-b.example.com"},
	}
	candidates := hostnameCandidates(m)
	// MachineHostName address overrides the NodeRef name; infra name and
	// machine name follow.
	want := []string{"host-b.example.com", "cp-1-infra", "cp-1"}
	if len(candidates) != len(want) {
		t.Fatalf("candidates = %v, want %v", candidates, want)
	}
	for i := range want {
		if candidates[i] != want[i] {
			t.Errorf("candidates[%d] = %q, want %q", i, candidates[i], want[i])
		}
	}
}

func TestMatchesMember(t *testing.T) {
	candidates := []string{"host-b.example.com", "cp-1"}
	for member, want := range map[string]bool{
		"HOST-B":          true, // FQDN-trimmed, case-insensitive
		"host-b.internal": true,
		"cp-1":            true,
		"cp-2":            false,
		"host-b-2":        false,
	} {
		if got := matchesMember(candidates, member); got != want {
			t.Errorf("matchesMember(%q) = %v, want %v", member, got, want)
		}
	}
}
