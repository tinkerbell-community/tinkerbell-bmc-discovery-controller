package sync

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/bmc-toolbox/common"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
)

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"X570D4I-2T", "x570d4i-2t"},
		{"My BMC (2)", "my-bmc-2"},
		{"already-valid", "already-valid"},
		{"---", ""},
		{"", ""},
		{strings.Repeat("a", 70), strings.Repeat("a", 63)},
		{strings.Repeat("a", 62) + "--suffix", strings.Repeat("a", 62)},
	}
	for _, tt := range tests {
		if got := SanitizeName(tt.in); got != tt.want {
			t.Errorf("SanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTemplateName(t *testing.T) {
	ep := mdns.Endpoint{
		Instance: "obmc_console on x570d4i2t",
		Hostname: "x570d4i2t.local.",
		IP:       netip.MustParseAddr("10.0.80.1"),
	}
	dev := testDevice() // primary MAC aa:bb:cc:dd:ee:01, serial SN12345

	tests := []struct {
		name string
		tmpl string
		want string
	}{
		{"cluster-mac", "talos-${mac}", "talos-aabbccddee01"},
		{"mac with dashes", "talos-${mac_dashes}", "talos-aa-bb-cc-dd-ee-01"},
		{"hostname", "${hostname}-bmc", "x570d4i2t-bmc"},
		{"serial", "node-${serial}", "node-sn12345"},
		{"ip", "bmc-${ip}", "bmc-10-0-80-1"},
		{"empty template", "", ""},
		{"unknown variable", "x-${nope}", ""},
		{"literal only", "static-name", "static-name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TemplateName(tt.tmpl, ep, dev); got != tt.want {
				t.Errorf("TemplateName(%q) = %q, want %q", tt.tmpl, got, tt.want)
			}
		})
	}

	// A template referencing the MAC of a device without NICs is unresolvable.
	empty := common.NewDevice()
	if got := TemplateName("talos-${mac}", ep, &empty); got != "" {
		t.Errorf("TemplateName with no MAC = %q, want empty (fallback)", got)
	}
}

func TestResourceName(t *testing.T) {
	ip := netip.MustParseAddr("10.0.80.1")
	tests := []struct {
		name string
		ep   mdns.Endpoint
		want string
	}{
		{
			// The hostname is service-independent, so a BMC advertising
			// several service types maps to one resource name.
			name: "hostname first, domain stripped",
			ep:   mdns.Endpoint{Instance: "obmc_console on x570d4i2t", Hostname: "x570d4i2t.local.", IP: ip},
			want: "x570d4i2t",
		},
		{
			name: "falls back to instance",
			ep:   mdns.Endpoint{Instance: "X570D4I-2T", Hostname: "", IP: ip},
			want: "x570d4i-2t",
		},
		{
			name: "falls back to IP",
			ep:   mdns.Endpoint{Instance: "()", Hostname: "--", IP: ip},
			want: "bmc-10-0-80-1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResourceName(tt.ep); got != tt.want {
				t.Errorf("ResourceName(%+v) = %q, want %q", tt.ep, got, tt.want)
			}
		})
	}
}
