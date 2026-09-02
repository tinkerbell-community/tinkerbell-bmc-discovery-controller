package teardown

import (
	"strings"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// hostnameCandidates builds the victim's possible etcd member hostnames,
// mirroring CACPPT auditEtcd (controllers/etcd.go:220-246) and extended for
// machines that never joined the cluster:
//
//  1. status.nodeRef.name, overridden by the first MachineHostName address.
//  2. spec.infrastructureRef.name — under P6 and CABPT
//     hostname.source=InfrastructureName, the TinkerbellMachine name IS the
//     hostname (== hardware name).
//  3. metadata.name — covers hostname.source=MachineName.
func hostnameCandidates(m *clusterv1.Machine) []string {
	var candidates []string
	nodeName := m.Status.NodeRef.Name
	for _, addr := range m.Status.Addresses {
		if addr.Type == clusterv1.MachineHostName {
			nodeName = addr.Address
			break
		}
	}
	for _, c := range []string{nodeName, m.Spec.InfrastructureRef.Name, m.Name} {
		if c != "" {
			candidates = append(candidates, c)
		}
	}
	return candidates
}

// matchesMember compares a member hostname against the candidate set: both
// sides are truncated at the first "." (FQDN trim) and compared
// case-insensitively, exactly as CACPPT etcd.go:238-241. Names are unique
// per cluster by CABPT construction, so multiple candidates cannot match
// different members.
func matchesMember(candidates []string, memberHostname string) bool {
	member, _, _ := strings.Cut(memberHostname, ".")
	for _, c := range candidates {
		candidate, _, _ := strings.Cut(c, ".")
		if strings.EqualFold(candidate, member) {
			return true
		}
	}
	return false
}

// findMember returns the etcd member matching the candidate set, or nil.
func findMember(resp *machineapi.EtcdMemberListResponse, candidates []string) *machineapi.EtcdMember {
	if resp == nil {
		return nil
	}
	for _, msg := range resp.GetMessages() {
		for _, member := range msg.GetMembers() {
			if matchesMember(candidates, member.GetHostname()) {
				return member
			}
		}
	}
	return nil
}

// etcdRunningState is the Talos service state of a live etcd.
const etcdRunningState = "Running"

// etcdHealthy reports whether a ServiceInfo answer describes a running,
// healthy etcd service.
func etcdHealthy(svcs []talosclient.ServiceInfo) bool {
	for _, svc := range svcs {
		info := svc.Service
		if info == nil {
			continue
		}
		if info.GetState() == etcdRunningState && info.GetHealth().GetHealthy() {
			return true
		}
	}
	return false
}

// etcdNotFinished reports whether any etcd service is in a state other than
// Finished — CACPPT's gracefulEtcdLeave gate for leave-via-victim
// (etcd.go:127-136): a Finished etcd has already left.
func etcdNotFinished(svcs []talosclient.ServiceInfo) bool {
	for _, svc := range svcs {
		if svc.Service != nil && svc.Service.GetState() != "Finished" {
			return true
		}
	}
	return false
}
