package sync

import (
	"regexp"
	"strings"

	"github.com/bmc-toolbox/common"
	bmcv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/inventory"
	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
)

const (
	// ManagedByLabel marks resources owned by this controller. Resources
	// without it are never modified.
	ManagedByLabel = "discovery.tinkerbell.org/managed-by"
	// ManagedByValue is the value of ManagedByLabel on owned resources.
	ManagedByValue = "tinkerbell-bmc-discovery-controller"
	// FieldManager is the server-side-apply field manager for every write
	// this controller makes. It deliberately equals ManagedByValue: SSA
	// manager identity == component name (repo naming standard, see
	// docs/discovery-field-ownership.md).
	FieldManager = ManagedByValue
	// LastSeenAnnotation records the last successful sync time (RFC3339).
	LastSeenAnnotation = "discovery.tinkerbell.org/last-seen"
	// InstanceAnnotation records the mDNS instance name as advertised.
	InstanceAnnotation = "discovery.tinkerbell.org/mdns-instance"
	// ServiceAnnotation records the DNS-SD service type the BMC advertised.
	ServiceAnnotation = "discovery.tinkerbell.org/mdns-service"
)

// macPattern matches the lowercase MAC format required by the Hardware CRD.
var macPattern = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// The Desired* builders return sparse apply configurations: TypeMeta (the
// apply patch body must carry apiVersion/kind), name/namespace, and ONLY the
// fields discovery owns. The object is marshaled verbatim as the SSA payload,
// so a field left unset here is a field discovery does not own — foreign
// writers' fields (Hardware spec.userData from CAPT,
// spec.metadata.instance.operating_system and .state from talos-os-metadata /
// tinkerbell-hardware-janitor, spec.vendorData, spec.references,
// spec.resources, spec.tinkVersion) must never be added.

// DesiredAuthSecret builds the sparse BMC auth Secret.
func DesiredAuthSecret(name, namespace string, creds inventory.Credentials) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			"username": []byte(creds.Username),
			"password": []byte(creds.Password),
		},
	}
}

// DesiredMachine builds the sparse bmc.tinkerbell.org Machine for a
// discovered Redfish endpoint.
func DesiredMachine(ep mdns.Endpoint, name, namespace string, insecureTLS bool, authRef corev1.SecretReference) *bmcv1.Machine {
	return &bmcv1.Machine{
		TypeMeta:   metav1.TypeMeta{APIVersion: bmcv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: bmcv1.MachineSpec{
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
		},
	}
}

// HardwareOptions parameterize the Hardware built from inventory.
type HardwareOptions struct {
	// Name is the resource name, shared by the linked Machine; it becomes
	// the instance and DHCP hostname.
	Name string
	// Namespace is the resource namespace.
	Namespace string
	// FacilityCode fills metadata.facility.facility_code when non-empty.
	FacilityCode string
	// AutoEnrollment enables Tinkerbell auto enrollment for the Hardware.
	// Note: AutoCapabilities.EnrollmentEnabled is a non-pointer bool with
	// omitempty, so false serializes as `auto: {}` and the field is removed
	// on apply; absent equals false for all consumers.
	AutoEnrollment bool
}

// DesiredHardware builds the sparse tinkerbell.org Hardware from BMC
// inventory, following the conventions of hand-provisioned Hardware:
// agentID and metadata.instance.id are the primary in-band MAC (serial only
// when no MAC is known) and the primary interface carries the DHCP hostname.
// live carries the existing netboot values forward (see netbootFor); pass
// nil on create.
func DesiredHardware(dev *common.Device, opts HardwareOptions, live *tinkv1.Hardware) *tinkv1.Hardware {
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
	// Only the primary ethernet interface is recorded, even when the device
	// reports several — matching hand-provisioned Hardware, where one
	// netboot interface identifies the machine. spec.interfaces is an
	// SSA-atomic list (no listType marker upstream), so discovery owns the
	// whole list and must carry foreign netboot state forward inside it.
	if mac := PrimaryMAC(dev); mac != "" {
		spec.Interfaces = []tinkv1.Interface{{
			Netboot: netbootFor(live),
			DHCP:    &tinkv1.DHCP{MAC: mac, Hostname: opts.Name},
		}}
	}
	for _, drive := range dev.Drives {
		if drive != nil && drive.LogicalName != "" {
			spec.Disks = append(spec.Disks, tinkv1.Disk{Device: drive.LogicalName})
		}
	}
	return &tinkv1.Hardware{
		TypeMeta:   metav1.TypeMeta{APIVersion: tinkv1.GroupVersion.String(), Kind: "Hardware"},
		ObjectMeta: metav1.ObjectMeta{Name: opts.Name, Namespace: opts.Namespace},
		Spec:       spec,
	}
}

// netbootFor implements the netboot carry-forward rule: discovery authors
// allowPXE/allowWorkflow defaults at create only and never changes netboot
// afterward — the tink workflow controller owns PXE arming during workflow
// runs, and discovery's apply must not undo its allowPXE=false disarm. On
// update the live primary interface's netboot is serialized verbatim
// (ownership without authorship, forced by the atomic interfaces list).
func netbootFor(live *tinkv1.Hardware) *tinkv1.Netboot {
	if live != nil && len(live.Spec.Interfaces) > 0 && live.Spec.Interfaces[0].Netboot != nil {
		return live.Spec.Interfaces[0].Netboot.DeepCopy()
	}
	return &tinkv1.Netboot{
		AllowPXE:      ptr.To(true),
		AllowWorkflow: ptr.To(true),
	}
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
