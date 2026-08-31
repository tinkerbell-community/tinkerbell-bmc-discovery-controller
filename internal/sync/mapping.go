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

// HardwareOptions parameterize the Hardware spec built from inventory.
type HardwareOptions struct {
	// Name is the resource name, shared by the linked Machine; it becomes
	// the instance and DHCP hostname.
	Name string
	// FacilityCode fills metadata.facility.facility_code when non-empty.
	FacilityCode string
	// AutoEnrollment enables Tinkerbell auto enrollment for the Hardware.
	AutoEnrollment bool
}

// DesiredHardwareSpec builds the tinkerbell.org Hardware spec from BMC
// inventory, following the conventions of hand-provisioned Hardware:
// agentID and metadata.instance.id are the primary in-band MAC (serial only
// when no MAC is known), every interface is netboot-enabled, and the primary
// interface carries the DHCP hostname.
func DesiredHardwareSpec(dev *common.Device, opts HardwareOptions) tinkv1.HardwareSpec {
	instanceID := PrimaryMAC(dev)
	if instanceID == "" {
		instanceID = dev.Serial
	}
	spec := tinkv1.HardwareSpec{
		AgentID: PrimaryMAC(dev),
		Auto:    tinkv1.AutoCapabilities{EnrollmentEnabled: opts.AutoEnrollment},
		BMCRef: &corev1.TypedLocalObjectReference{
			APIGroup: ptr.To("bmc.tinkerbell.org/v1alpha1"),
			Kind:     "Machine",
			Name:     opts.Name,
		},
		Metadata: &tinkv1.HardwareMetadata{
			Manufacturer: &tinkv1.MetadataManufacturer{Slug: SanitizeName(dev.Vendor)},
			Instance: &tinkv1.MetadataInstance{
				ID:       instanceID,
				Hostname: opts.Name,
			},
		},
	}
	if opts.FacilityCode != "" {
		spec.Metadata.Facility = &tinkv1.MetadataFacility{FacilityCode: opts.FacilityCode}
	}
	for i, mac := range inbandMACs(dev) {
		iface := tinkv1.Interface{
			Netboot: &tinkv1.Netboot{
				AllowPXE:      ptr.To(true),
				AllowWorkflow: ptr.To(true),
			},
			DHCP: &tinkv1.DHCP{MAC: mac},
		}
		if i == 0 {
			iface.DHCP.Hostname = opts.Name
		}
		spec.Interfaces = append(spec.Interfaces, iface)
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
