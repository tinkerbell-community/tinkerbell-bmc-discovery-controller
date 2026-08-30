package sync

import (
	"net/netip"
	"strings"
	"testing"

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
