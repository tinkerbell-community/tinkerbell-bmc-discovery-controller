// Package sync maps discovered BMC endpoints and inventory onto Tinkerbell
// Machine, Hardware, and Secret resources and keeps them up to date.
package sync

import (
	"regexp"
	"strings"

	"github.com/bmc-toolbox/common"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
)

// templateVar matches ${name} references in a name template.
var templateVar = regexp.MustCompile(`\$\{([a-z_]+)\}`)

// TemplateName renders a name template like "talos-${mac}" from the endpoint
// and its verified inventory, then sanitizes the result. Supported variables:
// ${mac} (primary in-band MAC, colons removed), ${mac_dashes} (colons as
// dashes), ${hostname} (first mDNS hostname label), ${instance}, ${serial},
// ${ip}. It returns "" — meaning the caller should fall back to
// ResourceName — when the template is empty or any referenced variable is
// unknown or resolves to nothing.
func TemplateName(tmpl string, ep mdns.Endpoint, dev *common.Device) string {
	if tmpl == "" {
		return ""
	}
	mac := PrimaryMAC(dev)
	host, _, _ := strings.Cut(ep.Hostname, ".")
	vars := map[string]string{
		"mac":        strings.ReplaceAll(mac, ":", ""),
		"mac_dashes": strings.ReplaceAll(mac, ":", "-"),
		"hostname":   host,
		"instance":   ep.Instance,
		"serial":     dev.Serial,
		"ip":         strings.NewReplacer(".", "-", ":", "-").Replace(ep.IP.String()),
	}
	unresolved := false
	out := templateVar.ReplaceAllStringFunc(tmpl, func(ref string) string {
		value := vars[templateVar.FindStringSubmatch(ref)[1]]
		if value == "" {
			unresolved = true
		}
		return value
	})
	if unresolved {
		return ""
	}
	return SanitizeName(out)
}

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

// ResourceName derives the stable resource name for an endpoint: the first
// label of the sanitized mDNS hostname (hostnames are unique on the link and
// identical across every service type a BMC advertises), falling back to the
// instance name, then to the IP address. The name never derives from
// inventory (e.g. serial), so it is identical before and after inventory
// collection succeeds.
func ResourceName(ep mdns.Endpoint) string {
	host, _, _ := strings.Cut(ep.Hostname, ".")
	if name := SanitizeName(host); name != "" {
		return name
	}
	if name := SanitizeName(ep.Instance); name != "" {
		return name
	}
	return "bmc-" + SanitizeName(strings.NewReplacer(".", "-", ":", "-").Replace(ep.IP.String()))
}
