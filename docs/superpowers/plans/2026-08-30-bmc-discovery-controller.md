# tinkerbell-bmc-discovery-controller Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Kubernetes controller that discovers BMCs via mDNS, collects Redfish inventory, and creates/updates Tinkerbell `Machine` (bmc.tinkerbell.org) and `Hardware` (tinkerbell.org) resources.

**Architecture:** A single controller-runtime manager with one Runnable worker: an mDNS browser emits endpoints on a channel; a workqueue-driven worker collects inventory via bmclib and upserts Machine/Hardware/auth-Secret with adoption-label safety. No new CRDs.

**Tech Stack:** Go ≥1.26.3, `sigs.k8s.io/controller-runtime v0.24.1`, `k8s.io/* v0.36.3`, `github.com/tinkerbell/tinkerbell` (API types), `github.com/bmc-toolbox/bmclib/v2`, `github.com/bmc-toolbox/common`, `github.com/libp2p/zeroconf/v2`.

**Spec:** `docs/superpowers/specs/2026-08-30-bmc-discovery-controller-design.md`

## Global Constraints

- Module path: `github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller`
- go directive: `go 1.26.3` (tinkerbell dependency floor)
- Ownership label: `discovery.tinkerbell.org/managed-by: tinkerbell-bmc-discovery-controller`; never modify resources lacking it
- Annotations: `discovery.tinkerbell.org/last-seen` (RFC3339), `discovery.tinkerbell.org/mdns-instance`, `discovery.tinkerbell.org/mdns-service`
- Resource names: sanitized mDNS instance name (RFC 1123), NEVER the serial
- mDNS disappearance never deletes resources
- Default service types: `_redfish._tcp`, `_obmc_redfish._tcp`; domain `local.`
- Image: `ghcr.io/tinkerbell-community/tinkerbell-bmc-discovery-controller`
- License: Apache-2.0 (copy from tinkerbell repo)

---

### Task 1: Repo scaffolding

**Files:**
- Create: `go.mod`, `.gitignore`, `LICENSE`
- Already present: `docs/superpowers/specs/…-design.md`, this plan

**Steps:**
- [ ] `go mod init github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller`; set `go 1.26.3`
- [ ] `.gitignore`: `bin/`, `cover.out`, `dist/`
- [ ] Copy `LICENSE` from `/home/appkins/src/tinkerbell-community/tinkerbell/LICENSE`
- [ ] `go get` deps: `sigs.k8s.io/controller-runtime@v0.24.1`, `github.com/tinkerbell/tinkerbell@latest`, `github.com/bmc-toolbox/bmclib/v2@latest`, `github.com/bmc-toolbox/common@latest`, `github.com/libp2p/zeroconf/v2@latest`, `github.com/go-logr/logr`
- [ ] Commit: `chore: scaffold module, license, design doc, plan`

### Task 2: internal/mdns — Endpoint type and zeroconf browser

**Files:**
- Create: `internal/mdns/browser.go`, `internal/mdns/browser_test.go`

**Interfaces (Produces):**
```go
type Endpoint struct {
    Instance string // mDNS instance name (unique on the network)
    Service  string // e.g. "_redfish._tcp"
    Hostname string // mDNS hostname, e.g. "bmc.local."
    IP       netip.Addr
    Port     int
}
func (e Endpoint) Key() string // Service + "/" + Instance

type Browser interface {
    Run(ctx context.Context, events chan<- Endpoint) error
}

type ZeroconfBrowser struct {
    Log          logr.Logger
    ServiceTypes []string
    Domain       string
    Interval     time.Duration // between browse cycles
    Window       time.Duration // duration of one browse
}
func EntryToEndpoint(e *zeroconf.ServiceEntry) (Endpoint, bool)
```

- [ ] Write failing tests for `EntryToEndpoint`: prefers IPv4 over IPv6; falls back to IPv6; returns false with no addresses; carries instance/service/hostname/port. And `Endpoint.Key()`.
- [ ] Run `go test ./internal/mdns/` — expect FAIL (undefined symbols)
- [ ] Implement `EntryToEndpoint`, `Key`, and `ZeroconfBrowser.Run` (loop: `browseOnce` then wait `Interval`; `browseOnce` browses each service type inside a `Window` timeout context, forwarding entries; `zeroconf.Browse` errors are logged, and the entries channel is closed manually only on Browse error since Browse closes it on success path)
- [ ] `go test ./internal/mdns/` — expect PASS; `go vet ./...`
- [ ] Commit: `feat: mDNS browser emitting BMC endpoints`

### Task 3: internal/sync — name sanitization

**Files:**
- Create: `internal/sync/names.go`, `internal/sync/names_test.go`

**Interfaces (Produces):**
```go
func SanitizeName(s string) string           // RFC 1123 label, "" if nothing survives
func ResourceName(ep mdns.Endpoint) string   // sanitize(Instance) → sanitize(Hostname) → "bmc-"+ip-with-dashes
```

- [ ] Failing tests: `"X570D4I-2T"→"x570d4i-2t"`; `"My BMC (2)"→"my-bmc-2"`; 70-char input truncated to 63 without trailing dash; `"---"→""`; ResourceName fallback chain (instance → hostname → IP form `bmc-10-0-80-1`)
- [ ] Run tests — FAIL
- [ ] Implement (lowercase; non `[a-z0-9-]` → `-`; collapse runs of `-`; trim `-`; truncate 63 then trim again)
- [ ] Run tests — PASS
- [ ] Commit: `feat: RFC1123 resource naming from mDNS identity`

### Task 4: internal/sync — spec mapping

**Files:**
- Create: `internal/sync/mapping.go`, `internal/sync/mapping_test.go`

**Interfaces (Produces):**
```go
const (
    ManagedByLabel     = "discovery.tinkerbell.org/managed-by"
    ManagedByValue     = "tinkerbell-bmc-discovery-controller"
    LastSeenAnnotation = "discovery.tinkerbell.org/last-seen"
    InstanceAnnotation = "discovery.tinkerbell.org/mdns-instance"
    ServiceAnnotation  = "discovery.tinkerbell.org/mdns-service"
)
func DesiredMachineSpec(ep mdns.Endpoint, insecureTLS bool, authRef corev1.SecretReference) bmcv1.MachineSpec
func DesiredHardwareSpec(dev *common.Device, bmcName string) tinkv1.HardwareSpec
func PrimaryMAC(dev *common.Device) string   // first valid in-band NIC MAC, lowercased
```
(imports: `bmcv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"`, `tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"`)

- [ ] Failing tests: MachineSpec has Host=ep.IP, Port=ep.Port, InsecureTLS, authRef, ProviderOptions{PreferredOrder:["gofish"], Redfish:{Port: ep.Port, UseBasicAuth: true}}; HardwareSpec has AgentID = first valid MAC lowercased, one Interface per unique valid MAC (`^([0-9a-f]{2}:){5}[0-9a-f]{2}$` after lowering; invalid/dup skipped), Disks only from drives with LogicalName, Metadata.Manufacturer.Slug = SanitizeName(vendor), Metadata.Instance{ID: serial, Hostname: bmcName}, BMCRef{APIGroup:"bmc.tinkerbell.org", Kind:"Machine", Name: bmcName}
- [ ] Run — FAIL
- [ ] Implement
- [ ] Run — PASS
- [ ] Commit: `feat: map mDNS endpoint + inventory to Machine/Hardware specs`

### Task 5: internal/inventory — credentials + bmclib collector

**Files:**
- Create: `internal/inventory/collector.go`

**Interfaces (Produces):**
```go
type Credentials struct{ Username, Password string }

type Collector interface {
    Collect(ctx context.Context, ep mdns.Endpoint, creds Credentials) (*common.Device, error)
}

type BMCLibCollector struct {
    Timeout time.Duration
    Log     logr.Logger
}
```

- [ ] Implement `BMCLibCollector.Collect`: `bmclib.NewClient(ep.IP.String(), user, pass, WithLogger, WithRedfishPort(strconv.Itoa(ep.Port)), WithRedfishUseBasicAuth(true))`; `client.Registry.Drivers = client.Registry.PreferDriver("gofish")`; Open with Timeout ctx; `defer Close`; `Inventory(ctx)` (thin network boundary — no unit test; covered by worker fakes)
- [ ] `go build ./...` + `go vet ./...` pass
- [ ] Commit: `feat: Redfish inventory collector via bmclib`

### Task 6: internal/sync — Syncer upserts with adoption safety

**Files:**
- Create: `internal/sync/syncer.go`, `internal/sync/syncer_test.go`

**Interfaces (Consumes):** Tasks 3–5 (`ResourceName`, `Desired*Spec`, `inventory.Credentials`).
**Interfaces (Produces):**
```go
type Syncer struct {
    Client      client.Client
    Namespace   string
    InsecureTLS bool
    Now         func() time.Time
    Log         logr.Logger
}
// dev may be nil: Machine + auth Secret only, no Hardware
func (s *Syncer) Sync(ctx context.Context, ep mdns.Endpoint, dev *common.Device, creds inventory.Credentials) error
```

Behavior: name := ResourceName(ep); upsert Secret `<name>-bmc-auth` (data username/password), Machine `<name>`, and (if dev != nil) Hardware `<name>` via `controllerutil.CreateOrUpdate`; mutate fn returns sentinel `errUnmanaged` when the object exists without the managed-by label (caught → log + skip, nil error); every managed object gets ManagedBy label + the three annotations (last-seen from `s.Now()`).

- [ ] Failing tests with `fake.NewClientBuilder` (scheme: corev1 + bmcv1 + tinkv1 AddToScheme):
  1. full sync creates Secret+Machine+Hardware with expected spec/labels/annotations
  2. dev=nil creates Secret+Machine only
  3. pre-existing unlabeled Machine is untouched and no error returned
  4. second Sync with later `Now` updates last-seen and corrects spec drift
- [ ] Run — FAIL
- [ ] Implement
- [ ] Run — PASS
- [ ] Commit: `feat: upsert Machine/Hardware/auth Secret with adoption safety`

### Task 7: internal/controller — discovery worker

**Files:**
- Create: `internal/controller/worker.go`, `internal/controller/worker_test.go`

**Interfaces (Consumes):** `mdns.Browser`, `inventory.Collector`, `*sync.Syncer`.
**Interfaces (Produces):**
```go
type Worker struct {
    Client            client.Client
    Browser           mdns.Browser
    Collector         inventory.Collector
    Syncer            *syncpkg.Syncer
    CredentialsSecret types.NamespacedName
    ResyncInterval    time.Duration
    Log               logr.Logger
    // internal: mutex-guarded known map[string]mdns.Endpoint
}
func (w *Worker) Start(ctx context.Context) error        // manager.Runnable
func (w *Worker) NeedLeaderElection() bool               // true
```

Behavior: `Start` launches Browser.Run into a buffered channel and a queue processor over `workqueue.TypedRateLimitingInterface[string]`; events record endpoint in `known` and enqueue key; resync ticker re-enqueues all known keys; `handle(key)` reads credentials Secret (keys `username`/`password`; error if missing), calls Collector (on error: log, proceed with dev=nil, requeue after Sync), then Syncer.Sync; collect/sync errors → `AddRateLimited`, success → `Forget`.

- [ ] Failing tests (fake Browser emitting scripted endpoints, fake Collector, fake client preloaded with creds Secret, real Syncer, short resync; poll-with-timeout helper instead of sleeps):
  1. endpoint event → Machine+Hardware+auth Secret appear
  2. collector error → Machine appears, Hardware absent
  3. missing username key in creds Secret → nothing created, no panic
- [ ] Run — FAIL
- [ ] Implement
- [ ] Run — PASS (with `-race`)
- [ ] Commit: `feat: discovery worker wiring browser, collector, syncer`

### Task 8: cmd/main.go + Makefile

**Files:**
- Create: `cmd/main.go`, `Makefile`

Flags (spec table): `--namespace` (default `tink`), `--service-types` (comma list, default `_redfish._tcp,_obmc_redfish._tcp`), `--mdns-domain` (`local.`), `--browse-interval` (5m), `--browse-window` (30s), `--resync-interval` (1h), `--credentials-secret` (`bmc-discovery-credentials`), `--collect-timeout` (2m), `--insecure-tls` (true), `--leader-elect` (false), `--metrics-bind-address` (`:8080`), `--health-probe-bind-address` (`:8081`), zap flags.

- [ ] Implement main: zap logger; scheme (clientgoscheme + bmcv1 + tinkv1); manager with cache `DefaultNamespaces: {namespace: {}}`, leader election id `tinkerbell-bmc-discovery-controller.discovery.tinkerbell.org`; construct ZeroconfBrowser, BMCLibCollector, Syncer, Worker; `mgr.Add(worker)`; healthz/readyz ping checks
- [ ] Makefile targets: `build`, `test` (`go test -race -coverprofile=cover.out ./...`), `vet`, `fmt-check`, `docker-build`, `helm-lint`
- [ ] `make build test vet` all pass
- [ ] Commit: `feat: manager entrypoint and Makefile`

### Task 9: Dockerfile + GitHub Actions

**Files:**
- Create: `Dockerfile`, `.github/workflows/ci.yaml`, `.github/workflows/release.yaml`

- [ ] Dockerfile: `golang:1.26` build stage (`CGO_ENABLED=0 go build -o /out/manager ./cmd`), final `gcr.io/distroless/static:nonroot`, `ENTRYPOINT ["/manager"]`
- [ ] `ci.yaml`: on push to main + PRs — checkout, setup-go (`go-version-file: go.mod`), `make vet fmt-check test build`
- [ ] `release.yaml`: on push to main + tags `v*` — docker/login-action to ghcr.io with GITHUB_TOKEN, docker/metadata-action (tags: branch, semver, sha), docker/build-push-action push
- [ ] `docker build .` succeeds locally
- [ ] Commit: `ci: docker image build and GitHub Actions workflows`

### Task 10: Helm chart

**Files:**
- Create: `helm/tinkerbell-bmc-discovery-controller/{Chart.yaml,values.yaml,templates/{_helpers.tpl,serviceaccount.yaml,rbac.yaml,deployment.yaml,credentials-secret.yaml}}`

- [ ] Chart.yaml (apiVersion v2, appVersion "0.1.0"); values per spec flag table plus `image.{repository,tag,pullPolicy}`, `credentials.{create,name,username,password}`, `hostNetwork: true`, `leaderElection: true`, `resources`, `nodeSelector`, `tolerations`
- [ ] Deployment: hostNetwork + `dnsPolicy: ClusterFirstWithHostNet`, args from values, probes on 8081, nonroot securityContext
- [ ] RBAC Role (namespace-scoped): secrets + machines (bmc.tinkerbell.org) + hardware (tinkerbell.org): get/list/watch/create/update/patch; leases: get/create/update; events: create/patch
- [ ] credentials-secret.yaml rendered only when `credentials.create`
- [ ] `helm lint` + `helm template` render clean; template output shows hostNetwork and expected args
- [ ] Commit: `feat: helm chart`

### Task 11: README

**Files:**
- Create: `README.md`

- [ ] Sections: what it does (diagram from spec), how discovery→inventory→resources flows, prerequisites (tinkerbell CRDs installed, BMC advertising mDNS, shared BMC credentials Secret), helm install example, configuration table, adoption/deletion semantics, development (make targets)
- [ ] Commit: `docs: README`

### Task 12: Publish

- [ ] `gh repo create tinkerbell-community/tinkerbell-bmc-discovery-controller --public --description "Discovers BMCs via mDNS and manages Tinkerbell BMC Machine and Hardware resources from their Redfish inventory" --disable-wiki`
- [ ] `git remote add origin https://github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller.git && git push -u origin main`
- [ ] Add `../tinkerbell-community/tinkerbell-bmc-discovery-controller` folder entry to `/home/appkins/src/sidero-community/CapiFullStack.code-workspace`
- [ ] Verify CI runs green on GitHub (`gh run watch`)
