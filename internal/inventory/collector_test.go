package inventory

import (
	"log/slog"
	"net/netip"
	"testing"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
)

func TestNewBMCClientDrivers(t *testing.T) {
	ep := mdns.Endpoint{
		Instance: "X570D4I-2T",
		Service:  "_obmc_redfish._tcp",
		IP:       netip.MustParseAddr("10.0.80.1"),
		Port:     443,
	}
	client := newBMCClient(ep, Credentials{Username: "admin", Password: "pw"}, slog.New(slog.DiscardHandler))

	var names []string
	for _, driver := range client.Registry.Drivers {
		names = append(names, driver.Name)
	}
	if len(names) == 0 {
		t.Fatal("no drivers registered")
	}
	if names[0] != "gofish" {
		t.Errorf("first driver = %q, want gofish preferred; drivers: %v", names[0], names)
	}
	hasIPMI := false
	for _, name := range names {
		if name == "ipmitool" {
			t.Errorf("exec-based ipmitool driver must not be registered; drivers: %v", names)
		}
		if name == "ipmi" {
			hasIPMI = true
		}
	}
	if !hasIPMI {
		t.Errorf("native go-ipmi driver missing; drivers: %v", names)
	}
}
