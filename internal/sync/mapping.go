package sync

import (
	"regexp"
	"strings"

	"github.com/bmc-toolbox/common"
	bmcv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
)

const (
	// ManagedByLabel marks resources owned by this controller. Resources
	// without it are never modified.
	ManagedByLabel = "discovery.tinkerbell.org/managed-by"
	// ManagedByValue is the value of ManagedByLabel on owned resources.
	ManagedByValue = "tinkerbell-bmc-discovery-controller"
	// LastSeenAnnotation records the last successful sync time (RFC3339).
	LastSeenAnnotation = "discovery.tinkerbell.org/last-seen"
	// InstanceAnnotation records the mDNS instance name as advertised.
	InstanceAnnotation = "discovery.tinkerbell.org/mdns-instance"
	// ServiceAnnotation records the DNS-SD service type the BMC advertised.
	ServiceAnnotation = "discovery.tinkerbell.org/mdns-service"
)

// macPattern matches the lowercase MAC format required by the Hardware CRD.
var macPattern = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// DesiredMachineSpec builds the bmc.tinkerbell.org Machine spec for a
// discovered Redfish endpoint.
func DesiredMachineSpec(ep mdns.Endpoint, insecureTLS bool, authRef corev1.SecretReference) bmcv1.MachineSpec {
	return bmcv1.MachineSpec{
		Connection: bmcv1.Connection{
			Host:          ep.IP.String(),
			Port:          ep.Port,
			AuthSecretRef: authRef,
			InsecureTLS:   insecureTLS,
			ProviderOptions: &bmcv1.ProviderOptions{
				PreferredOrder: []bmcv1.ProviderName{"gofish"},
				Redfish: &bmcv1.RedfishOptions{
					Port:         ep.Port,
					UseBasicAuth: true,
				},
			},
		},
	}
}

// DesiredHardwareSpec builds the tinkerbell.org Hardware spec from BMC
// inventory. bmcName is the Machine resource linked via spec.bmcRef.
func DesiredHardwareSpec(dev *common.Device, bmcName string) tinkv1.HardwareSpec {
	spec := tinkv1.HardwareSpec{
		AgentID: PrimaryMAC(dev),
		BMCRef: &corev1.TypedLocalObjectReference{
			APIGroup: ptr.To("bmc.tinkerbell.org"),
			Kind:     "Machine",
			Name:     bmcName,
		},
		Metadata: &tinkv1.HardwareMetadata{
			Manufacturer: &tinkv1.MetadataManufacturer{Slug: SanitizeName(dev.Vendor)},
			Instance: &tinkv1.MetadataInstance{
				ID:       dev.Serial,
				Hostname: bmcName,
			},
		},
	}
	for _, mac := range inbandMACs(dev) {
		spec.Interfaces = append(spec.Interfaces, tinkv1.Interface{
			DHCP: &tinkv1.DHCP{MAC: mac},
		})
	}
	for _, drive := range dev.Drives {
		if drive != nil && drive.LogicalName != "" {
			spec.Disks = append(spec.Disks, tinkv1.Disk{Device: drive.LogicalName})
		}
	}
	return spec
}

// PrimaryMAC returns the first valid in-band NIC MAC, lowercased, or "".
func PrimaryMAC(dev *common.Device) string {
	if macs := inbandMACs(dev); len(macs) > 0 {
		return macs[0]
	}
	return ""
}

// inbandMACs lists the unique, valid, lowercased MACs of the device's
// in-band NIC ports (the BMC's own NIC is not included in dev.NICs).
func inbandMACs(dev *common.Device) []string {
	var macs []string
	seen := map[string]bool{}
	for _, nic := range dev.NICs {
		if nic == nil {
			continue
		}
		for _, port := range nic.NICPorts {
			if port == nil {
				continue
			}
			mac := strings.ToLower(port.MacAddress)
			if !macPattern.MatchString(mac) || seen[mac] {
				continue
			}
			seen[mac] = true
			macs = append(macs, mac)
		}
	}
	return macs
}
