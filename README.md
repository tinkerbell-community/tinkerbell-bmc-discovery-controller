# tinkerbell-bmc-discovery-controller

A Kubernetes controller that discovers Baseboard Management Controllers (BMCs)
on the local network via mDNS/DNS-SD, collects their hardware inventory over
Redfish, and manages [Tinkerbell](https://tinkerbell.org) resources from it:

- **`Machine`** (`bmc.tinkerbell.org/v1alpha1`) — BMC connection details, ready
  for power/boot management by Tinkerbell's rufio controller.
- **`Hardware`** (`tinkerbell.org/v1alpha1`) — machine identity (agent ID,
  NIC MACs, disks, manufacturer/serial metadata), linked to its Machine via
  `spec.bmcRef`.

No new CRDs are introduced; the controller only manages existing Tinkerbell
types.

## How it works

```text
┌──────────────────────────── manager ────────────────────────────┐
│                                                                 │
│  ┌───────────────┐   events    ┌──────────────────────────────┐ │
│  │ mDNS browser  │ ──────────► │ discovery worker             │ │
│  │ (_redfish._tcp│  channel    │  1. resolve endpoint         │ │
│  │  _obmc_redfish│             │  2. collect inventory        │ │
│  │  ._tcp)       │             │     (Redfish via bmclib)     │ │
│  └───────────────┘             │  3. upsert Machine, Hardware,│ │
│                                │     auth Secret              │ │
│                                └──────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

1. The browser continuously browses configurable DNS-SD service types.
   OpenBMC advertises `_obmc_redfish._tcp` via Avahi; standard Redfish
   services advertise `_redfish._tcp`.
2. For each discovered endpoint the worker reads the shared BMC credentials
   Secret, collects inventory over Redfish, and upserts:
   - a per-machine auth Secret `<name>-bmc-auth` (username/password),
   - a `Machine` pointing at the BMC (created even when inventory collection
     fails — connection details are known from mDNS alone),
   - a `Hardware` populated from inventory (only once collection succeeds).
3. Everything re-syncs periodically; failed collections retry with backoff.

Resource names derive from the sanitized first label of the mDNS hostname
(e.g. `x570d4i2t.local` → `x570d4i2t`), so they are stable across restarts,
identical for every service type a BMC advertises, and available before
inventory collection succeeds.

### Discovering BMCs that only advertise a console service

Some OpenBMC builds do not advertise Redfish over mDNS at all — only, say,
`_obmc_console._tcp` (whose advertised port is the SOL console, not Redfish).
Discovery can ride any advertisement the BMC does publish; pin the Redfish
port explicitly:

```yaml
discovery:
  serviceTypes:
    - _obmc_console._tcp
  redfishPort: 443
```

### Ownership and deletion semantics

- Every resource the controller creates carries the label
  `discovery.tinkerbell.org/managed-by: tinkerbell-bmc-discovery-controller`.
- Pre-existing resources **without** that label are never modified: an
  existing hand-written Machine with the same name is left alone and logged.
- A BMC disappearing from mDNS **never deletes** its resources (mDNS presence
  is flaky); the `discovery.tinkerbell.org/last-seen` annotation simply stops
  advancing.
- Additional annotations record the discovery source:
  `discovery.tinkerbell.org/mdns-instance` and
  `discovery.tinkerbell.org/mdns-service`.

## Prerequisites

- The Tinkerbell CRDs (`hardware.tinkerbell.org`, `machines.bmc.tinkerbell.org`)
  installed — they ship with the [tinkerbell helm chart](https://github.com/tinkerbell/tinkerbell).
- BMCs advertising Redfish over mDNS, reachable from the node the controller
  runs on. By default the pod uses `hostNetwork: true` because mDNS needs
  multicast (see below for the Multus alternative).
- A Secret with the shared BMC credentials (keys `username` and `password`).

## Install

```sh
kubectl -n tink create secret generic bmc-discovery-credentials \
  --from-literal=username=admin --from-literal=password='...'

helm install bmc-discovery \
  ./helm/tinkerbell-bmc-discovery-controller \
  --namespace tink
```

### Non-host networking via Multus ipvlan

Instead of host networking, the pod can get a dedicated IPv4 interface on the
BMC VLAN through [Multus](https://github.com/k8snetworkplumbingwg/multus-cni)
with the ipvlan CNI. The chart creates the NetworkAttachmentDefinition,
annotates the pod, and pins mDNS browsing to the attached interface:

```yaml
networking:
  multus:
    enabled: true          # turns hostNetwork off
    master: eth0           # node uplink carrying the BMC VLAN
    ipam:                  # any CNI IPAM config; the pod needs an IPv4
      type: host-local     # address on the BMC network
      subnet: 10.0.80.0/24
      rangeStart: 10.0.80.240
      rangeEnd: 10.0.80.250
```

Discovery itself is IPv4-only (BMC IPv6 advertisements are typically
link-local or ULA addresses unreachable from the cluster).

## Configuration

| Flag | Helm value | Default | Purpose |
| --- | --- | --- | --- |
| `--namespace` | release namespace | `tink` | Namespace for created resources |
| `--service-types` | `discovery.serviceTypes` | `_redfish._tcp,_obmc_redfish._tcp` | DNS-SD types to browse |
| `--mdns-domain` | `discovery.domain` | `local.` | Browse domain |
| `--mdns-interfaces` | `networking.multus.interface` (when enabled) | all interfaces | Interfaces to browse on |
| `--browse-interval` | `discovery.browseInterval` | `5m` | Time between browse cycles |
| `--browse-window` | `discovery.browseWindow` | `30s` | Duration of each browse |
| `--resync-interval` | `discovery.resyncInterval` | `1h` | Inventory refresh for known BMCs |
| `--collect-timeout` | `discovery.collectTimeout` | `2m` | Timeout per inventory collection |
| `--credentials-secret` | `credentials.name` | `bmc-discovery-credentials` | Secret with `username`/`password` |
| `--redfish-port` | `discovery.redfishPort` | `0` (advertised port) | Redfish port override for non-Redfish advertisements |
| `--insecure-tls` | `insecureTLS` | `true` | Skip BMC TLS verification |
| `--leader-elect` | `leaderElection` | `false` (chart: `true`) | Leader election |

## Development

```sh
make build      # compile to bin/manager
make test       # go test -race ./...
make vet        # go vet
make lint       # golangci-lint run
make snapshot   # goreleaser local build (binaries + images, no publish)
make helm-lint
```

Design and implementation notes live in
[docs/superpowers/specs](docs/superpowers/specs/) and
[docs/superpowers/plans](docs/superpowers/plans/).

## License

[Apache 2.0](LICENSE)
