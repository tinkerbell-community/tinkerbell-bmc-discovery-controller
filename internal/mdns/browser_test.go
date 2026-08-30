package mdns

import (
	"net"
	"testing"

	"github.com/libp2p/zeroconf/v2"
)

func TestEntryToEndpoint(t *testing.T) {
	entry := func(v4, v6 []net.IP) *zeroconf.ServiceEntry {
		e := &zeroconf.ServiceEntry{
			HostName: "bmc.local.",
			Port:     443,
			AddrIPv4: v4,
			AddrIPv6: v6,
		}
		e.Instance = "X570D4I-2T"
		e.Service = "_redfish._tcp"
		e.Domain = "local"
		return e
	}

	tests := []struct {
		name   string
		entry  *zeroconf.ServiceEntry
		wantOK bool
		wantIP string
	}{
		{
			name:   "uses IPv4",
			entry:  entry([]net.IP{net.ParseIP("10.0.80.1")}, []net.IP{net.ParseIP("fe80::1")}),
			wantOK: true,
			wantIP: "10.0.80.1",
		},
		{
			name:   "ignores IPv6-only entries",
			entry:  entry(nil, []net.IP{net.ParseIP("fe80::1")}),
			wantOK: false,
		},
		{
			name:   "no addresses",
			entry:  entry(nil, nil),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, ok := EntryToEndpoint(tt.entry)
			if ok != tt.wantOK {
				t.Fatalf("EntryToEndpoint ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got := ep.IP.String(); got != tt.wantIP {
				t.Errorf("IP = %q, want %q", got, tt.wantIP)
			}
			if ep.Instance != "X570D4I-2T" || ep.Service != "_redfish._tcp" || ep.Hostname != "bmc.local." || ep.Port != 443 {
				t.Errorf("unexpected endpoint fields: %+v", ep)
			}
		})
	}
}

func TestEndpointKey(t *testing.T) {
	ep := Endpoint{Instance: "X570D4I-2T", Service: "_redfish._tcp"}
	if got, want := ep.Key(), "_redfish._tcp/X570D4I-2T"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
}
