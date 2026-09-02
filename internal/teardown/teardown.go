// Package teardown implements talos-machine-teardown (C3): a
// hook-implementing controller that gives every CAPI Machine deletion path a
// talosctl-like teardown — etcd membership removal for control-plane nodes
// and a Talos Reset (wipe STATE+EPHEMERAL, then halt) for all nodes —
// strictly before CAPT powers the hardware off, using the pre-terminate
// machine deletion hook annotation. See docs/talos-machine-teardown.md.
package teardown

import (
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const (
	// Name is the component name: binary, chart, image, field manager,
	// event recorder, and hook annotation value (repo naming standard).
	Name = "talos-machine-teardown"

	// HookAnnotation is the CAPI pre-terminate machine deletion hook C3
	// implements. The key suffix is stable across component renames.
	HookAnnotation = clusterv1.PreTerminateDeleteHookAnnotationPrefix + "/talos-teardown"
	// HookValue names the owning controller, per the deletion-hook proposal.
	HookValue = Name

	// ObservedAtAnnotation records when C3 first observed the pre-terminate
	// wait; all phase deadlines are measured from it so controller restarts
	// do not reset the clock.
	ObservedAtAnnotation = "teardown.tinkerbell.org/pre-terminate-observed-at"
	// EtcdAnnotation records the etcd phase conclusion.
	EtcdAnnotation = "teardown.tinkerbell.org/etcd"
	// ResetAnnotation records the reset phase conclusion.
	ResetAnnotation = "teardown.tinkerbell.org/reset"

	// ClusterNameLabel and ClusterNamespaceLabel mark C3's cached
	// talosconfig secrets with their source cluster.
	ClusterNameLabel      = "teardown.tinkerbell.org/cluster-name"
	ClusterNamespaceLabel = "teardown.tinkerbell.org/cluster-namespace"

	// EtcdLeavingAnnotation is CACPPT's scale-down marker. It means "leave
	// ATTEMPTED", never proof of completion (it is stamped before the leave
	// runs, and a failed leave is silently swallowed by the scale.go
	// leaveErr bug) — C3 treats it strictly as a hint to skip the
	// leadership-forfeit nicety.
	EtcdLeavingAnnotation = "controlplane.cluster.x-k8s.io/etcd-leaving"
)

// etcd phase conclusions (values of EtcdAnnotation).
const (
	EtcdNotMember            = "not-member"
	EtcdLeft                 = "left"
	EtcdRemoved              = "removed"
	EtcdOrphaned             = "orphaned"
	EtcdSkippedClusterDelete = "skipped-cluster-delete"
)

// reset phase conclusions (values of ResetAnnotation).
const (
	ResetDone             = "done"
	ResetSkippedTimeout   = "skipped-timeout"
	ResetSkippedNoAddress = "skipped-no-address"
)

// Event reasons emitted on Machines.
const (
	EventHookStamped              = "TeardownHookStamped"
	EventEtcdMemberLeft           = "EtcdMemberLeft"
	EventEtcdMemberRemoved        = "EtcdMemberRemoved"
	EventEtcdMemberOrphaned       = "EtcdMemberOrphaned"
	EventEtcdSkippedClusterDelete = "EtcdSkippedClusterDelete"
	EventResetSucceeded           = "TalosResetSucceeded"
	EventResetSkipped             = "TalosResetSkipped"
	EventTeardownComplete         = "TeardownComplete"
	EventCredentialsCached        = "TeardownCredentialsCached"
	EventCredentialsMissing       = "TeardownCredentialsMissing"
)
