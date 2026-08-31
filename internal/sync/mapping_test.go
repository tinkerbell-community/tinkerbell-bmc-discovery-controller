package sync

import (
	"net/netip"
	"testing"

	"github.com/bmc-toolbox/common"
	bmcv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	corev1 "k8s.io/api/core/v1"

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

func TestDesiredMachineSpec(t *testing.T) {
	authRef := corev1.SecretReference{Name: "x570d4i-2t-bmc-auth", Namespace: "tink"}
	spec := DesiredMachineSpec(testEndpoint(), true, authRef)

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

func TestDesiredHardwareSpec(t *testing.T) {
	spec := DesiredHardwareSpec(testDevice(), HardwareOptions{
		Name:           "talos-aabbccddee01",
		FacilityCode:   "onprem",
		AutoEnrollment: true,
	})

	// agentID and metadata.instance.id are the primary in-band MAC, matching
	// the convention of hand-provisioned Hardware in this environment.
	if spec.AgentID != "aa:bb:cc:dd:ee:01" {
		t.Errorf("AgentID = %q, want aa:bb:cc:dd:ee:01", spec.AgentID)
	}
	if !spec.Auto.EnrollmentEnabled {
		t.Error("Auto.EnrollmentEnabled = false, want true")
	}

	if len(spec.Interfaces) != 2 {
		t.Fatalf("Interfaces count = %d, want 2 (deduped, invalid skipped)", len(spec.Interfaces))
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
		t.Errorf("Interfaces[0].Netboot = %+v, want allowPXE and allowWorkflow true", first.Netboot)
	}
	second := spec.Interfaces[1]
	if second.DHCP == nil || second.DHCP.MAC != "aa:bb:cc:dd:ee:02" || second.DHCP.Hostname != "" {
		t.Errorf("Interfaces[1] = %+v, want MAC aa:bb:cc:dd:ee:02 and no hostname", second)
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

func TestDesiredHardwareSpecNoMACFallsBackToSerial(t *testing.T) {
	dev := common.NewDevice()
	dev.Serial = "SN99"
	spec := DesiredHardwareSpec(&dev, HardwareOptions{Name: "x570d4i2t"})

	if spec.AgentID != "" {
		t.Errorf("AgentID = %q, want empty without MACs", spec.AgentID)
	}
	if spec.Metadata == nil || spec.Metadata.Instance == nil || spec.Metadata.Instance.ID != "SN99" {
		t.Errorf("Instance = %+v, want ID SN99 (serial fallback)", spec.Metadata)
	}
	if spec.Metadata.Facility != nil {
		t.Errorf("Facility = %+v, want nil when no facility code configured", spec.Metadata.Facility)
	}
}
