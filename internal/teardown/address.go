package teardown

import (
	"context"
	"fmt"

	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TinkerbellMachineGVK identifies the infra machine. It is read as
// unstructured — only three fields are needed, and importing the CAPT fork's
// api module would force a replace directive on every consumer (the same
// trade-off talos-upgrade-coordinator records for TalosControlPlane).
var TinkerbellMachineGVK = schema.GroupVersionKind{
	Group:   "infrastructure.cluster.x-k8s.io",
	Version: "v1beta2",
	Kind:    "TinkerbellMachine",
}

// resolveAddress returns the node address for a Machine via the fallback
// chain of docs/talos-machine-teardown.md: Machine.status.addresses →
// TinkerbellMachine.status.addresses (present even for machines that never
// joined) → Hardware DHCP IP (covers a TinkerbellMachine whose status was
// never populated). An empty result with nil error means the chain is
// exhausted: the node never had an address anywhere and there is nothing to
// dial.
func (r *Reconciler) resolveAddress(ctx context.Context, m *clusterv1.Machine) (string, error) {
	for _, addr := range m.Status.Addresses {
		if addr.Type == clusterv1.MachineInternalIP || addr.Type == clusterv1.MachineExternalIP {
			return addr.Address, nil
		}
	}

	if m.Spec.InfrastructureRef.Name == "" {
		return "", nil
	}
	tm := &unstructured.Unstructured{}
	tm.SetGroupVersionKind(TinkerbellMachineGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.InfrastructureRef.Name}, tm)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("getting TinkerbellMachine %s: %w", m.Spec.InfrastructureRef.Name, err)
	}

	addresses, _, _ := unstructured.NestedSlice(tm.Object, "status", "addresses")
	for _, entry := range addresses {
		addr, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		addrType, _ := addr["type"].(string)
		if addrType == string(clusterv1.MachineInternalIP) || addrType == string(clusterv1.MachineExternalIP) {
			if value, _ := addr["address"].(string); value != "" {
				return value, nil
			}
		}
	}

	return r.hardwareAddress(ctx, tm)
}

// hardwareAddress reads the Hardware DHCP IP referenced by a
// TinkerbellMachine.
func (r *Reconciler) hardwareAddress(ctx context.Context, tm *unstructured.Unstructured) (string, error) {
	hardwareName, _, _ := unstructured.NestedString(tm.Object, "spec", "hardwareName")
	if hardwareName == "" {
		return "", nil
	}
	namespace, _, _ := unstructured.NestedString(tm.Object, "status", "targetNamespace")
	if namespace == "" {
		namespace = tm.GetNamespace()
	}

	var hw tinkv1.Hardware
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: hardwareName}, &hw)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("getting Hardware %s/%s: %w", namespace, hardwareName, err)
	}
	if len(hw.Spec.Interfaces) == 0 {
		return "", nil
	}
	dhcp := hw.Spec.Interfaces[0].DHCP
	if dhcp == nil || dhcp.IP == nil {
		return "", nil
	}
	return dhcp.IP.Address, nil
}
