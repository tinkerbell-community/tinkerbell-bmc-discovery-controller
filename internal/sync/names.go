// Package sync maps discovered BMC endpoints and inventory onto Tinkerbell
// Machine, Hardware, and Secret resources and keeps them up to date.
package sync

import (
	"strings"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
)

// SanitizeName lowers s into an RFC 1123 DNS label: lowercase alphanumerics
// and dashes, at most 63 characters, no leading or trailing dash. It returns
// "" when nothing survives.
func SanitizeName(s string) string {
	var b strings.Builder
	lastDash := true // suppress leading dashes
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	return out
}

// ResourceName derives the stable resource name for an endpoint: the
// sanitized mDNS instance name, falling back to the mDNS hostname, then to
// the IP address. The name never derives from inventory (e.g. serial), so it
// is identical before and after inventory collection succeeds.
func ResourceName(ep mdns.Endpoint) string {
	if name := SanitizeName(ep.Instance); name != "" {
		return name
	}
	if name := SanitizeName(ep.Hostname); name != "" {
		return name
	}
	return "bmc-" + SanitizeName(strings.NewReplacer(".", "-", ":", "-").Replace(ep.IP.String()))
}
