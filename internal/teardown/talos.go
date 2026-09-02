package teardown

import (
	"context"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

// Client is the narrow seam over the Talos machinery client used by the
// teardown flows, kept minimal so tests can script node behavior.
type Client interface {
	ServiceInfo(ctx context.Context, service string) ([]talosclient.ServiceInfo, error)
	EtcdMemberList(ctx context.Context, req *machineapi.EtcdMemberListRequest) (*machineapi.EtcdMemberListResponse, error)
	EtcdForfeitLeadership(ctx context.Context, req *machineapi.EtcdForfeitLeadershipRequest) (*machineapi.EtcdForfeitLeadershipResponse, error)
	EtcdLeaveCluster(ctx context.Context, req *machineapi.EtcdLeaveClusterRequest) error
	EtcdRemoveMemberByID(ctx context.Context, req *machineapi.EtcdRemoveMemberByIDRequest) error
	ResetGeneric(ctx context.Context, req *machineapi.ResetRequest) error
	Close() error
}

// ClientFactory dials a Talos node at endpoint using the cluster's client
// configuration. The returned client is scoped to that single node.
type ClientFactory func(ctx context.Context, cfg *talosconfig.Config, endpoint string) (Client, error)

// NewTalosClient is the production ClientFactory, mirroring CACPPT's
// construction (controllers/configs.go): the cluster talosconfig supplies
// identity, the endpoint list is overridden with the single target node.
func NewTalosClient(ctx context.Context, cfg *talosconfig.Config, endpoint string) (Client, error) {
	c, err := talosclient.New(ctx,
		talosclient.WithConfig(cfg),
		talosclient.WithEndpoints(endpoint),
		talosclient.WithDefaultGRPCDialOptions(),
	)
	if err != nil {
		return nil, err
	}
	return &realClient{Client: c}, nil
}

// realClient adapts *talosclient.Client to the Client seam.
type realClient struct {
	*talosclient.Client
}

func (c *realClient) ServiceInfo(ctx context.Context, service string) ([]talosclient.ServiceInfo, error) {
	return c.Client.ServiceInfo(ctx, service)
}

func (c *realClient) EtcdMemberList(ctx context.Context, req *machineapi.EtcdMemberListRequest) (*machineapi.EtcdMemberListResponse, error) {
	return c.Client.EtcdMemberList(ctx, req)
}

func (c *realClient) EtcdForfeitLeadership(ctx context.Context, req *machineapi.EtcdForfeitLeadershipRequest) (*machineapi.EtcdForfeitLeadershipResponse, error) {
	return c.Client.EtcdForfeitLeadership(ctx, req)
}

func (c *realClient) EtcdLeaveCluster(ctx context.Context, req *machineapi.EtcdLeaveClusterRequest) error {
	return c.Client.EtcdLeaveCluster(ctx, req)
}

func (c *realClient) EtcdRemoveMemberByID(ctx context.Context, req *machineapi.EtcdRemoveMemberByIDRequest) error {
	return c.Client.EtcdRemoveMemberByID(ctx, req)
}

func (c *realClient) ResetGeneric(ctx context.Context, req *machineapi.ResetRequest) error {
	return c.Client.ResetGeneric(ctx, req)
}

func (c *realClient) Close() error {
	return c.Client.Close()
}
