package inventory

import (
	"fmt"
	"strings"
)

// WildcardService is the ParseDefaults key whose credentials apply to any
// service type without a more specific entry.
const WildcardService = "*"

// ParseDefaults parses per-service-type fallback credentials from a
// comma-separated list of "<service>=<username>:<password>" entries, e.g.
// "*=admin:admin,_obmc_console._tcp=root:0penBmc". The service "*" is the
// wildcard fallback for any service type. The password may contain colons
// and equals signs, but not commas.
func ParseDefaults(s string) (map[string]Credentials, error) {
	defaults := map[string]Credentials{}
	if s == "" {
		return defaults, nil
	}
	for _, entry := range strings.Split(s, ",") {
		service, cred, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("default credentials entry %q: want <service>=<username>:<password>", entry)
		}
		username, password, ok := strings.Cut(cred, ":")
		if !ok || service == "" || username == "" || password == "" {
			return nil, fmt.Errorf("default credentials entry %q: want <service>=<username>:<password>", entry)
		}
		defaults[service] = Credentials{Username: username, Password: password}
	}
	return defaults, nil
}
