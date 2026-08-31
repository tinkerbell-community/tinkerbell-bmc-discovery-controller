# tinkerbell-bmc-discovery-controller — Design

Date: 2026-08-30
Status: Approved (architecture approved by @appkins; repo name, visibility, and
approach confirmed via interactive question)

## Purpose

A Kubernetes controller that discovers Baseboard Management Controllers (BMCs)
on the local network via mDNS/DNS-SD, collects hardware inventory from each
discovered BMC over Redfish, and creates/updates Tinkerbell resources from that
inventory:

- `Machine` (`bmc.tinkerbell.org/v1alpha1`) — BMC connection details.
- `Hardware` (`tinkerbell.org/v1alpha1`) — machine identity, NIC interfaces,
  disks, and metadata, with `spec.bmcRef` linking to the Machine.

No new CRDs are introduced. The controller manages existing Tinkerbell types,
imported from `github.com/tinkerbell/tinkerbell`.

## Constraints and context

- BMCs in this environment run OpenBMC (e.g. ASRock Rack X570D4I-2T), which
  advertises `_obmc_redfish._tcp` via Avahi. Standard Redfish services
  advertise `_redfish._tcp`. Both are browsed by default; the list is
  configurable.
- mDNS requires multicast on the host network, so the controller pod runs with
  `hostNetwork: true`.
- mDNS presence is flaky. Disappearance from mDNS NEVER deletes Machine or
  Hardware resources; it only updates a last-seen annotation.

## Architecture

A single Go binary built on `sigs.k8s.io/controller-runtime`:

```
┌──────────────────────────── manager ────────────────────────────┐
│                                                                 │
│  ┌───────────────┐   events    ┌──────────────────────────────┐ │
│  │ mDNS browser  │ ──────────► │ discovery worker             │ │
│  │ (Runnable,    │  channel    │  1. resolve endpoint         │ │
│  │  periodic     │             │  2. collect inventory (Redfish│ │
│  │  re-browse)   │             │     via bmclib)              │ │
│  └───────────────┘             │  3. upsert Machine + Hardware│ │
│                                └──────────────────────────────┘ │
│  metrics / healthz / leader election                            │
└─────────────────────────────────────────────────────────────────┘
```

### Components

1. **mDNS browser** (`internal/mdns`)
   - Wraps `github.com/libp2p/zeroconf/v2` `Browse`.
   - Browses each configured service type on an interval (default 5m browse
     cycle, each browse window ~30s), deduplicates, and emits
     `Endpoint{Instance, Host, IP, Port, ServiceType}` on a channel.
   - Interface: `Browser` with `Run(ctx, chan<- Endpoint)`; the zeroconf
     dependency is isolated behind this interface so the worker is testable
     with a fake.

2. **Inventory collector** (`internal/inventory`)
   - Uses `github.com/bmc-toolbox/bmclib/v2` (same library Tinkerbell's rufio
     uses) with the gofish/Redfish provider to `Open` + `Inventory()`,
     returning `bmc-toolbox/common.Device` (vendor, model, serial, NICs with
     MACs, drives, CPU/memory).
   - Credentials come from a Secret (`--credentials-secret`, key/value =
     `username`/`password`) read at collect time; per-host credential Secrets
     may be added later (YAGNI now).
   - Interface: `Collector` with `Collect(ctx, Endpoint, Credentials)
     (*common.Device, error)`.

3. **Resource manager** (`internal/sync`)
   - Pure mapping functions (unit-tested):
     - endpoint+device → desired `MachineSpec` (host, port 443/redfish,
       authSecretRef pointing at a per-machine copy of the credentials Secret,
       providerOptions preferring gofish/redfish for OpenBMC).
     - device → desired `HardwareSpec` (agentID = primary in-band NIC MAC,
       interfaces with DHCP MAC entries, disks, metadata
       manufacturer/instance, bmcRef → Machine).
     - mDNS instance/serial → RFC 1123 DNS-safe resource name.
   - Upsert via `controllerutil.CreateOrUpdate` with the manager's client.
   - Ownership label `discovery.tinkerbell.org/managed-by:
     tinkerbell-bmc-discovery-controller` on everything it creates; fields on
     resources it did not create are never overwritten (it adopts only
     resources carrying the label; a pre-existing unlabeled Machine/Hardware
     with the same name is left alone and logged).
   - Annotations: `discovery.tinkerbell.org/last-seen` (RFC3339),
     `discovery.tinkerbell.org/mdns-instance`, `discovery.tinkerbell.org/mdns-service`.
   - Also creates the per-machine auth Secret (copy of the global credentials
     Secret) in the target namespace so `Machine.spec.connection.authSecretRef`
     is self-contained — matching rufio's expectation that the Secret contains
     `username`/`password` keys.

4. **Discovery worker** (`internal/controller`)
   - Consumes endpoint events, rate-limits per host (workqueue with backoff on
     collect errors), calls collector then sync.
   - Periodic full re-sync interval (default 1h) refreshes inventory for known
     endpoints.

### Naming

Revised 2026-08-31: an optional `--name-template` (e.g. `talos-${mac}`)
renders the resource name from endpoint + verified inventory (variables:
mac, mac_dashes, hostname, instance, serial, ip; result sanitized).
Unresolvable endpoints fall back to the default below. Hardware mapping was
also aligned with the environment's hand-provisioned convention: agentID and
metadata.instance.id = primary MAC (serial only without MACs), a single
netboot-enabled interface for the primary MAC only (2026-08-31: multiple
reported NICs no longer produce multiple interface entries),
facility_code and auto enrollment configurable, bmcRef.apiGroup
`bmc.tinkerbell.org/v1alpha1`, and a gofish Systems/EthernetInterfaces
fallback fills NIC MACs when bmclib inventory has none.

Default resource name: the sanitized (RFC 1123) first label of the mDNS hostname
(e.g. `x570d4i2t.local.` → `x570d4i2t`), falling back to the instance name,
then the IP. Hostnames are unique on the link, known before inventory
succeeds, and — unlike instance names — identical across every service type a
BMC advertises, so one BMC always maps to one resource set. (Revised
2026-08-30 from instance-first naming after observing a real OpenBMC
advertising only `_obmc_console._tcp` with instance name "obmc_console on
x570d4i2t".) The serial number is recorded in
`Hardware.spec.metadata.instance.id`, never used for naming. Machine and
Hardware share the same base name (different Kinds, no conflict), e.g.
Hardware `x570d4i2t`, Machine `x570d4i2t`, Secret `x570d4i2t-bmc-auth`.

### Redfish port override

`--redfish-port` (default 0 = use the advertised port) replaces the
mDNS-advertised port on every discovered endpoint. Required when discovery
rides a non-Redfish advertisement such as `_obmc_console._tcp`, whose
advertised port (2200, the SOL console) is not the Redfish port.

### Configuration (flags / helm values)

| Flag | Default | Purpose |
| --- | --- | --- |
| `--namespace` | `tink` | Namespace for created resources |
| `--service-types` | `_redfish._tcp,_obmc_redfish._tcp` | DNS-SD types to browse |
| `--mdns-domain` | `local.` | Browse domain |
| `--browse-interval` | `5m` | Time between browse cycles |
| `--browse-window` | `30s` | Duration of each browse |
| `--resync-interval` | `1h` | Inventory refresh for known hosts |
| `--credentials-secret` | `bmc-discovery-credentials` | name of Secret with `username`/`password` (read from the controller namespace) |
| `--insecure-tls` | `true` | Skip BMC TLS verification (self-signed BMC certs) |
| `--leader-elect`, `--metrics-bind-address`, `--health-probe-bind-address` | usual | controller-runtime standard |

### Error handling

- Collect failure (unreachable BMC, bad creds): logged, retried with backoff
  via workqueue; NO resources are created until the BMC connection is
  verified by a successful authenticated inventory collection. (Revised
  2026-08-30: originally a connection-only Machine was created from mDNS
  data alone; connection verification is now required first.)
- Credentials: an ordered candidate chain, pivoted through until one pair
  authenticates — the shared Secret first (when present and well-formed),
  then the service-type default, then the `*` catch-all
  (`--default-credentials`, preconfigured `*=admin:admin` plus OpenBMC's
  factory `root`/`0penBmc` for `_obmc_console._tcp`/`_obmc_redfish._tcp`).
  The per-machine auth Secret records whichever pair worked. (Revised
  2026-08-30 from Secret-else-single-default.)
- Upsert conflict: standard retry-on-conflict via CreateOrUpdate requeue.
- mDNS browse failure: logged, next cycle retries; browse failures never
  crash the manager.

### Testing

- Unit tests (table-driven, no network): name sanitization, spec mapping
  (device → HardwareSpec, endpoint → MachineSpec), adoption/label rules.
- Worker tests with fake Browser + fake Collector + controller-runtime fake
  client: endpoint event produces Machine, Machine+Hardware on successful
  inventory, no clobbering of unlabeled resources, last-seen updates.
- CI: `go test ./...`, `golangci-lint`, `go build`, docker build.

### Delivery

- `Dockerfile` (distroless, static build).
- Helm chart in `helm/tinkerbell-bmc-discovery-controller` (Deployment with
  `hostNetwork: true`, `dnsPolicy: ClusterFirstWithHostNet`, RBAC for
  machines/hardware/secrets, values for all flags).
- GitHub Actions: `ci.yaml` (test+lint on PR/push), `release.yaml` (image to
  `ghcr.io/tinkerbell-community/tinkerbell-bmc-discovery-controller` on main
  and tags).

## Out of scope (YAGNI)

- SSDP/WS-Discovery, IPMI scanning, subnet sweeps.
- Deleting resources when hosts disappear.
- Per-host credential mapping (single shared credentials Secret for now).
- A DiscoveredBMC CRD (explicitly rejected in design review).
- Workflow/OSIE orchestration — this controller only maintains inventory
  resources.
