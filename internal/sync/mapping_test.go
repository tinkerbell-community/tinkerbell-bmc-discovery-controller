package sync

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/bmc-toolbox/common"
	bmcv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller/internal/mdns"
)

func testEndpoint() mdns.Endpoint {
	return mdns.Endpoint{
		Instance: "X570D4I-2T",
		Service:  "_obmc_redfish._tcp",
		Hostname: "X570D4I-2T.local.",
		IP:       netip.MustParseAddr("10.0.80.1"),
		Port:     443,
	}
}

func testDevice() *common.Device {
	dev := common.NewDevice()
	dev.Vendor = "ASRockRack"
	dev.Model = "X570D4I-2T"
	dev.Serial = "SN12345"
	dev.NICs = []*common.NIC{
		{NICPorts: []*common.NICPort{
			{MacAddress: "AA:BB:CC:DD:EE:01"},
			{MacAddress: "aa:bb:cc:dd:ee:01"}, // duplicate of the first
		}},
		{NICPorts: []*common.NICPort{
			{MacAddress: "not-a-mac"},
			{MacAddress: "aa:bb:cc:dd:ee:02"},
		}},
	}
	dev.Drives = []*common.Drive{
		{Common: common.Common{LogicalName: "/dev/nvme0n1"}},
		{Common: common.Common{}}, // no logical name: skipped
	}
	return &dev
}

func testHardwareOptions() HardwareOptions {
	return HardwareOptions{
		Name:           "talos-aabbccddee01",
		Namespace:      "tink",
		FacilityCode:   "onprem",
		AutoEnrollment: true,
	}
}

func TestDesiredMachine(t *testing.T) {
	authRef := corev1.SecretReference{Name: "x570d4i-2t-bmc-auth", Namespace: "tink"}
	machine := DesiredMachine(testEndpoint(), "x570d4i-2t", "tink", true, authRef)

	if machine.APIVersion != "bmc.tinkerbell.org/v1alpha1" || machine.Kind != "Machine" {
		t.Errorf("TypeMeta = %s/%s, want bmc.tinkerbell.org/v1alpha1 Machine", machine.APIVersion, machine.Kind)
	}
	if machine.Name != "x570d4i-2t" || machine.Namespace != "tink" {
		t.Errorf("ObjectMeta = %s/%s", machine.Namespace, machine.Name)
	}
	spec := machine.Spec
	if spec.Connection.Host != "10.0.80.1" {
		t.Errorf("Host = %q, want 10.0.80.1", spec.Connection.Host)
	}
	if spec.Connection.Port != 443 {
		t.Errorf("Port = %d, want 443", spec.Connection.Port)
	}
	if !spec.Connection.InsecureTLS {
		t.Error("InsecureTLS = false, want true")
	}
	if spec.Connection.AuthSecretRef != authRef {
		t.Errorf("AuthSecretRef = %+v, want %+v", spec.Connection.AuthSecretRef, authRef)
	}
	opts := spec.Connection.ProviderOptions
	if opts == nil || opts.Redfish == nil {
		t.Fatalf("ProviderOptions.Redfish missing: %+v", opts)
	}
	if len(opts.PreferredOrder) != 1 || opts.PreferredOrder[0] != bmcv1.ProviderName("gofish") {
		t.Errorf("PreferredOrder = %v, want [gofish]", opts.PreferredOrder)
	}
	if opts.Redfish.Port != 443 || !opts.Redfish.UseBasicAuth {
		t.Errorf("Redfish = %+v, want Port 443 UseBasicAuth true", opts.Redfish)
	}
}

func TestPrimaryMAC(t *testing.T) {
	if got, want := PrimaryMAC(testDevice()), "aa:bb:cc:dd:ee:01"; got != want {
		t.Errorf("PrimaryMAC = %q, want %q", got, want)
	}
	empty := common.NewDevice()
	if got := PrimaryMAC(&empty); got != "" {
		t.Errorf("PrimaryMAC(empty) = %q, want empty", got)
	}
}

func TestDesiredHardware(t *testing.T) {
	hw := DesiredHardware(testDevice(), testHardwareOptions(), nil)

	if hw.APIVersion != "tinkerbell.org/v1alpha1" || hw.Kind != "Hardware" {
		t.Errorf("TypeMeta = %s/%s, want tinkerbell.org/v1alpha1 Hardware", hw.APIVersion, hw.Kind)
	}
	if hw.Name != "talos-aabbccddee01" || hw.Namespace != "tink" {
		t.Errorf("ObjectMeta = %s/%s", hw.Namespace, hw.Name)
	}
	spec := hw.Spec

	// agentID and metadata.instance.id are the primary in-band MAC, matching
	// the convention of hand-provisioned Hardware in this environment.
	if spec.AgentID != "aa:bb:cc:dd:ee:01" {
		t.Errorf("AgentID = %q, want aa:bb:cc:dd:ee:01", spec.AgentID)
	}
	if !spec.Auto.EnrollmentEnabled {
		t.Error("Auto.EnrollmentEnabled = false, want true")
	}

	// Only the primary ethernet interface is recorded, even when the device
	// reports several — matching hand-provisioned Hardware.
	if len(spec.Interfaces) != 1 {
		t.Fatalf("Interfaces count = %d, want 1 (primary only)", len(spec.Interfaces))
	}
	first := spec.Interfaces[0]
	if first.DHCP == nil || first.DHCP.MAC != "aa:bb:cc:dd:ee:01" {
		t.Errorf("Interfaces[0] = %+v, want DHCP MAC aa:bb:cc:dd:ee:01", first)
	}
	if first.DHCP.Hostname != "talos-aabbccddee01" {
		t.Errorf("Interfaces[0].DHCP.Hostname = %q, want the resource name", first.DHCP.Hostname)
	}
	if first.Netboot == nil || first.Netboot.AllowPXE == nil || !*first.Netboot.AllowPXE ||
		first.Netboot.AllowWorkflow == nil || !*first.Netboot.AllowWorkflow {
		t.Errorf("Interfaces[0].Netboot = %+v, want allowPXE and allowWorkflow true on create", first.Netboot)
	}

	if len(spec.Disks) != 1 || spec.Disks[0].Device != "/dev/nvme0n1" {
		t.Errorf("Disks = %+v, want one /dev/nvme0n1", spec.Disks)
	}

	md := spec.Metadata
	if md == nil || md.Manufacturer == nil || md.Manufacturer.Slug != "asrockrack" {
		t.Fatalf("Metadata.Manufacturer = %+v, want slug asrockrack", md)
	}
	if md.Instance == nil || md.Instance.ID != "aa:bb:cc:dd:ee:01" || md.Instance.Hostname != "talos-aabbccddee01" {
		t.Errorf("Metadata.Instance = %+v, want ID aa:bb:cc:dd:ee:01 Hostname talos-aabbccddee01", md.Instance)
	}
	if md.Facility == nil || md.Facility.FacilityCode != "onprem" {
		t.Errorf("Metadata.Facility = %+v, want facility_code onprem", md.Facility)
	}

	ref := spec.BMCRef
	if ref == nil || ref.APIGroup == nil || *ref.APIGroup != "bmc.tinkerbell.org/v1alpha1" || ref.Kind != "Machine" || ref.Name != "talos-aabbccddee01" {
		t.Errorf("BMCRef = %+v, want bmc.tinkerbell.org/v1alpha1 Machine talos-aabbccddee01", ref)
	}
}

func TestDesiredHardwareNoMACFallsBackToSerial(t *testing.T) {
	dev := common.NewDevice()
	dev.Serial = "SN99"
	hw := DesiredHardware(&dev, HardwareOptions{Name: "x570d4i2t", Namespace: "tink"}, nil)

	if hw.Spec.AgentID != "" {
		t.Errorf("AgentID = %q, want empty without MACs", hw.Spec.AgentID)
	}
	if hw.Spec.Metadata == nil || hw.Spec.Metadata.Instance == nil || hw.Spec.Metadata.Instance.ID != "SN99" {
		t.Errorf("Instance = %+v, want ID SN99 (serial fallback)", hw.Spec.Metadata)
	}
	if hw.Spec.Metadata.Facility != nil {
		t.Errorf("Facility = %+v, want nil when no facility code configured", hw.Spec.Metadata.Facility)
	}
	if len(hw.Spec.Interfaces) != 0 {
		t.Errorf("Interfaces = %+v, want none without a MAC", hw.Spec.Interfaces)
	}
}

// TestDesiredHardwareSparse locks in the safety property of the migration:
// the marshaled apply payload must never contain fields owned by other
// controllers (CAPT's userData, talos-os-metadata's operating_system and
// state, and the rest of the forbidden set from
// docs/discovery-field-ownership.md).
func TestDesiredHardwareSparse(t *testing.T) {
	live := &tinkv1.Hardware{Spec: tinkv1.HardwareSpec{
		UserData: ptr.To("#cloud-config foreign"),
		Interfaces: []tinkv1.Interface{{
			Netboot: &tinkv1.Netboot{AllowPXE: ptr.To(false), AllowWorkflow: ptr.To(true)},
		}},
		Metadata: &tinkv1.HardwareMetadata{Instance: &tinkv1.MetadataInstance{
			State:           "provisioned",
			OperatingSystem: &tinkv1.MetadataInstanceOperatingSystem{Slug: "abc123"},
		}},
	}}
	for name, l := range map[string]*tinkv1.Hardware{"create": nil, "update": live} {
		applyConfig, err := applyConfigurationFor(DesiredHardware(testDevice(), testHardwareOptions(), l))
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(applyConfig.Object)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			`"userData"`, `"vendorData"`, `"operating_system"`, `"state"`,
			`"references"`, `"resources"`, `"tinkVersion"`,
			`"status"`, `"creationTimestamp"`,
		} {
			if strings.Contains(string(payload), forbidden) {
				t.Errorf("%s payload contains forbidden field %s:\n%s", name, forbidden, payload)
			}
		}
	}
}

// TestNetbootCarryForward verifies the create-only authorship rule:
// defaults on create, the live values — including an explicit false —
// reproduced verbatim on update.
func TestNetbootCarryForward(t *testing.T) {
	if nb := netbootFor(nil); nb.AllowPXE == nil || !*nb.AllowPXE || nb.AllowWorkflow == nil || !*nb.AllowWorkflow {
		t.Errorf("netbootFor(nil) = %+v, want create defaults true/true", nb)
	}

	live := &tinkv1.Hardware{Spec: tinkv1.HardwareSpec{
		Interfaces: []tinkv1.Interface{{
			Netboot: &tinkv1.Netboot{AllowPXE: ptr.To(false), AllowWorkflow: ptr.To(true)},
		}},
	}}
	nb := netbootFor(live)
	if nb.AllowPXE == nil || *nb.AllowPXE || nb.AllowWorkflow == nil || !*nb.AllowWorkflow {
		t.Errorf("netbootFor(live) = %+v, want allowPXE false carried forward", nb)
	}
	if nb == live.Spec.Interfaces[0].Netboot {
		t.Error("netbootFor must deep-copy, not alias, the live netboot")
	}

	noNetboot := &tinkv1.Hardware{Spec: tinkv1.HardwareSpec{Interfaces: []tinkv1.Interface{{}}}}
	if nb := netbootFor(noNetboot); nb.AllowPXE == nil || !*nb.AllowPXE {
		t.Errorf("netbootFor(no netboot) = %+v, want create defaults", nb)
	}
}
