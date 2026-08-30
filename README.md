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

Resource names derive from the sanitized mDNS instance name, so they are
stable across restarts and across the inventory becoming available.

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
  runs on. The pod uses `hostNetwork: true` because mDNS needs multicast.
- A Secret with the shared BMC credentials (keys `username` and `password`).

## Install

```sh
kubectl -n tink create secret generic bmc-discovery-credentials \
  --from-literal=username=admin --from-literal=password='...'

helm install bmc-discovery \
  ./helm/tinkerbell-bmc-discovery-controller \
  --namespace tink
```

## Configuration

| Flag | Helm value | Default | Purpose |
| --- | --- | --- | --- |
| `--namespace` | release namespace | `tink` | Namespace for created resources |
| `--service-types` | `discovery.serviceTypes` | `_redfish._tcp,_obmc_redfish._tcp` | DNS-SD types to browse |
| `--mdns-domain` | `discovery.domain` | `local.` | Browse domain |
| `--browse-interval` | `discovery.browseInterval` | `5m` | Time between browse cycles |
| `--browse-window` | `discovery.browseWindow` | `30s` | Duration of each browse |
| `--resync-interval` | `discovery.resyncInterval` | `1h` | Inventory refresh for known BMCs |
| `--collect-timeout` | `discovery.collectTimeout` | `2m` | Timeout per inventory collection |
| `--credentials-secret` | `credentials.name` | `bmc-discovery-credentials` | Secret with `username`/`password` |
| `--insecure-tls` | `insecureTLS` | `true` | Skip BMC TLS verification |
| `--leader-elect` | `leaderElection` | `false` (chart: `true`) | Leader election |

## Development

```sh
make build      # compile to bin/manager
make test       # go test -race ./...
make vet        # go vet
make docker-build
make helm-lint
```

Design and implementation notes live in
[docs/superpowers/specs](docs/superpowers/specs/) and
[docs/superpowers/plans](docs/superpowers/plans/).

## License

[Apache 2.0](LICENSE)
