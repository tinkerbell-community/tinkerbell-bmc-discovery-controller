// Package inventory collects hardware inventory from BMCs over Redfish.
package inventory

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/bmc-toolbox/bmclib/v2"
	"github.com/bmc-toolbox/common"
	"github.com/go-logr/logr"
	"github.com/jacobweinstock/registrar"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
)

// Credentials authenticate against a BMC.
type Credentials struct {
	Username string
	Password string
}

// Collector retrieves the hardware inventory behind a discovered endpoint.
type Collector interface {
	Collect(ctx context.Context, ep mdns.Endpoint, creds Credentials) (*common.Device, error)
}

// BMCLibCollector collects inventory using bmclib's Redfish (gofish) provider.
type BMCLibCollector struct {
	Timeout time.Duration
	Log     logr.Logger
}

// newBMCClient builds a bmclib client for a discovered endpoint: Redfish
// (gofish) preferred, the exec-based ipmitool driver removed — the container
// image ships no ipmitool binary; IPMI fallback uses the pure-go "ipmi"
// (go-ipmi) provider instead.
func newBMCClient(ep mdns.Endpoint, creds Credentials, log logr.Logger) *bmclib.Client {
	client := bmclib.NewClient(ep.IP.String(), creds.Username, creds.Password,
		bmclib.WithLogger(log.WithValues("host", ep.IP.String())),
		bmclib.WithRedfishPort(strconv.Itoa(ep.Port)),
		bmclib.WithRedfishUseBasicAuth(true),
	)
	drivers := make(registrar.Drivers, 0, len(client.Registry.Drivers))
	for _, driver := range client.Registry.Drivers {
		if driver.Name != "ipmitool" {
			drivers = append(drivers, driver)
		}
	}
	client.Registry.Drivers = drivers
	client.Registry.Drivers = client.Registry.PreferDriver("gofish")
	return client
}

// Collect opens a Redfish session to the endpoint and returns its inventory.
func (c *BMCLibCollector) Collect(ctx context.Context, ep mdns.Endpoint, creds Credentials) (*common.Device, error) {
	client := newBMCClient(ep, creds, c.Log)

	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	if err := client.Open(ctx); err != nil {
		return nil, fmt.Errorf("opening BMC connection to %s: %w", ep.IP, err)
	}
	defer client.Close(ctx)

	dev, err := client.Inventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("collecting inventory from %s: %w", ep.IP, err)
	}
	return dev, nil
}
