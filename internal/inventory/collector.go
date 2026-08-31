// Package inventory collects hardware inventory from BMCs over Redfish.
package inventory

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/bmc-toolbox/bmclib/v2"
	"github.com/bmc-toolbox/common"
	"github.com/go-logr/logr"
	"github.com/jacobweinstock/registrar"
	"github.com/stmcginnis/gofish"

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
	Log     *slog.Logger
}

// newBMCClient builds a bmclib client for a discovered endpoint: Redfish
// (gofish) preferred, the exec-based ipmitool driver removed — the container
// image ships no ipmitool binary; IPMI fallback uses the pure-go "ipmi"
// (go-ipmi) provider instead.
func newBMCClient(ep mdns.Endpoint, creds Credentials, log *slog.Logger) *bmclib.Client {
	client := bmclib.NewClient(ep.IP.String(), creds.Username, creds.Password,
		bmclib.WithLogger(logr.FromSlogHandler(log.With("host", ep.IP.String()).Handler())),
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
	log := c.Log.With("host", ep.IP.String(), "port", ep.Port, "instance", ep.Instance)
	client := newBMCClient(ep, creds, c.Log)

	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	log.Debug("opening BMC connection", "username", creds.Username, "timeout", c.Timeout.String())
	if err := client.Open(ctx); err != nil {
		md := client.GetMetadata()
		log.Debug("BMC connection failed", "providersAttempted", md.ProvidersAttempted, "err", err)
		return nil, fmt.Errorf("opening BMC connection to %s: %w", ep.IP, err)
	}
	defer client.Close(ctx)
	md := client.GetMetadata()
	log.Info("BMC connection established", "provider", md.SuccessfulOpenConns)

	dev, err := client.Inventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("collecting inventory from %s: %w", ep.IP, err)
	}
	c.enrichNICs(ctx, ep, creds, dev)
	log.Info("inventory collected",
		"vendor", dev.Vendor, "model", dev.Model, "serial", dev.Serial,
		"nics", len(dev.NICs), "drives", len(dev.Drives), "cpus", len(dev.CPUs))
	return dev, nil
}

// enrichNICs fills dev.NICs from Redfish Systems EthernetInterfaces when
// bmclib's inventory found no NIC MACs — bmclib reads chassis
// NetworkAdapters, which many BMCs (OpenBMC included) do not populate for
// host NICs, while the host MACs live under Systems/EthernetInterfaces.
// Failures only log: enrichment is best-effort on top of a verified session.
func (c *BMCLibCollector) enrichNICs(ctx context.Context, ep mdns.Endpoint, creds Credentials, dev *common.Device) {
	if dev == nil || hasNICMAC(dev) {
		return
	}
	log := c.Log.With("host", ep.IP.String())
	client, err := gofish.ConnectContext(ctx, gofish.ClientConfig{
		Endpoint:  "https://" + net.JoinHostPort(ep.IP.String(), strconv.Itoa(ep.Port)),
		Username:  creds.Username,
		Password:  creds.Password,
		Insecure:  true,
		BasicAuth: true,
	})
	if err != nil {
		log.Debug("NIC enrichment: redfish connect failed", "err", err)
		return
	}
	defer client.Logout()

	systems, err := client.Service.Systems()
	if err != nil {
		log.Debug("NIC enrichment: listing systems failed", "err", err)
		return
	}
	for _, system := range systems {
		ifaces, err := system.EthernetInterfaces()
		if err != nil {
			log.Debug("NIC enrichment: listing ethernet interfaces failed", "system", system.ID, "err", err)
			continue
		}
		for _, iface := range ifaces {
			mac := iface.PermanentMACAddress
			if mac == "" {
				mac = iface.MACAddress
			}
			if mac == "" {
				continue
			}
			dev.NICs = append(dev.NICs, &common.NIC{
				ID:       iface.ID,
				NICPorts: []*common.NICPort{{MacAddress: mac}},
			})
		}
	}
	if hasNICMAC(dev) {
		log.Debug("NIC enrichment: host MACs found via Systems EthernetInterfaces", "nics", len(dev.NICs))
	}
}

// hasNICMAC reports whether any NIC port carries a MAC address.
func hasNICMAC(dev *common.Device) bool {
	for _, nic := range dev.NICs {
		if nic == nil {
			continue
		}
		for _, port := range nic.NICPorts {
			if port != nil && port.MacAddress != "" {
				return true
			}
		}
	}
	return false
}
