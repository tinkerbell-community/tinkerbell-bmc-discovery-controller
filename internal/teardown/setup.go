package teardown

import (
	"context"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/workqueue"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// SetupWithManager wires the controller: Machines (qualifying or hooked),
// Clusters (deletionTimestamp transitions flip machines into cluster-delete
// mode), and TinkerbellMachines (addresses appearing late) mapped to their
// owner Machines. The rate limiter implements the documented retry policy:
// 5s initial backoff doubling to a 30s cap.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, concurrency int) error {
	tinkerbellMachine := &unstructured.Unstructured{}
	tinkerbellMachine.SetGroupVersionKind(TinkerbellMachineGVK)

	return ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&clusterv1.Machine{}, builder.WithPredicates(machinePredicate())).
		Watches(&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.clusterToMachines),
			builder.WithPredicates(clusterDeletionPredicate())).
		Watches(tinkerbellMachine, handler.EnqueueRequestsFromMapFunc(infraMachineToMachine)).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: concurrency,
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
				5*time.Second, 30*time.Second),
		}).
		Complete(r)
}

// machinePredicate admits Machines this controller can service (or already
// annotated by it, so releases still reconcile after a qualification-
// breaking edit).
func machinePredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		machine, ok := obj.(*clusterv1.Machine)
		if !ok {
			return false
		}
		return Qualifies(machine) || HasHook(machine)
	})
}

// clusterDeletionPredicate admits only the Cluster events that flip
// machines into cluster-delete mode: a deletionTimestamp appearing (or
// already present), or the Cluster disappearing. Spec/status churn on
// healthy Clusters never fans out to Machine reconciles.
func clusterDeletionPredicate() predicate.Predicate {
	deleting := func(obj client.Object) bool {
		return obj != nil && !obj.GetDeletionTimestamp().IsZero()
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return deleting(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return deleting(e.ObjectNew) },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(e event.GenericEvent) bool { return deleting(e.Object) },
	}
}

// clusterToMachines maps a Cluster event to its qualifying Machines.
func (r *Reconciler) clusterToMachines(ctx context.Context, obj client.Object) []reconcile.Request {
	var machines clusterv1.MachineList
	if err := r.List(ctx, &machines, client.InNamespace(obj.GetNamespace()),
		client.MatchingLabels{clusterv1.ClusterNameLabel: obj.GetName()}); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range machines.Items {
		m := &machines.Items[i]
		if Qualifies(m) || HasHook(m) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(m)})
		}
	}
	return requests
}

// infraMachineToMachine maps a TinkerbellMachine to its owner Machine via
// owner references.
func infraMachineToMachine(_ context.Context, obj client.Object) []reconcile.Request {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "Machine" {
			return []reconcile.Request{{NamespacedName: client.ObjectKey{
				Namespace: obj.GetNamespace(), Name: ref.Name,
			}}}
		}
	}
	return nil
}

// sortByAge orders machines oldest first (deterministic tie-break on name).
func sortByAge(machines []clusterv1.Machine) {
	sort.Slice(machines, func(i, j int) bool {
		if machines[i].CreationTimestamp.Time.Equal(machines[j].CreationTimestamp.Time) {
			return machines[i].Name < machines[j].Name
		}
		return machines[i].CreationTimestamp.Time.Before(machines[j].CreationTimestamp.Time)
	})
}
