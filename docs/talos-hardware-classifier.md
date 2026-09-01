# talos-hardware-classifier (C1)

Status: Proposed — 2026-09-01

The talos-hardware-classifier is a plain controller that turns observed hardware inventory into
two machine-consumable facts: trait labels on `Hardware` (for pool selection via
`hardwareAffinity`) and the Talos system-extension annotation `talos.tinkerbell.org/system-extensions`
(the input CAPT's schematic resolution unions into the Image Factory schematic, CAPT fork
`pkg/schematic/schematic.go:35,68-81`). It closes the "extension selection is manual" gap without
ever touching schematics, Image Factory, machine configs, or `Hardware.spec` — its writes are
metadata-only (labels and annotations), applied with SSA field manager `talos-hardware-classifier`.
It also owns the ordering fix for the classification race: a completion marker label that the
documented pool convention requires in `hardwareAffinity`, so unclassified hardware is never
claimable and a schematic is never baked before classification finishes.

## Purpose and scope

- Derive platform traits (`gpu-vendor`, `cpu-vendor`, `board`) per `Hardware` from rufio's
  out-of-band inventory (`Hardware.status.attributes.outOfBand`, the only pre-render hardware
  signal — governing fact F6).
- Maintain the Hardware-side `talos.tinkerbell.org/system-extensions` annotation as its exclusive
  writer, with a three-way merge so C1 can retract entries it added while preserving entries it
  did not.
- Stamp the completion marker `classify.tinkerbell.org/classified` and force inventory refresh on
  first sight of unclassified hardware, shrinking the staleness window.
- Validate that classified hardware carrying a companion-config-requiring trait is actually
  covered by a pool that supplies that config (events + annotation, no silent GPU-without-modules
  nodes).
- Optionally assign `tinkerbell.org/role` via explicit, config-driven policy.
- Post-P2 only: emit `talos.tinkerbell.org/overlay` (and default `extra-kernel-args`) from board
  traits, flag-gated off until the CAPT fork consumes them.

Out of scope (owned elsewhere, see `docs/architecture.md` ownership matrix): schematic
resolution and Factory calls (CAPT, F2), `operating_system` metadata (C2), `Hardware.spec.userData`
(CAPT writes, C4 clears), any `TalosConfig`/`TalosControlPlane` mutation, teardown, upgrades.

## Context

Recap of the governing facts this component depends on (full statements in
`docs/architecture.md`, "Governing facts"): CAPT resolves one Image Factory schematic per machine
from hardware signals plus the `talos.tinkerbell.org/system-extensions` annotation on both
`Hardware` and `TinkerbellMachine`, then never re-resolves once the hardware is provisioned (F2);
rufio's out-of-band inventory refreshes on a 24h jitter and can be forced via the
`tinkerbell.org/refresh-inventory` annotation on the `bmc.tinkerbell.org` `Machine` (F6, verified:
`tinkerbell/rufio/internal/controller/inventory.go:49` — the annotation lives on the rufio
Machine, not on Hardware, and rufio clears it after a successful collection). Preconditions that
shape C1: P2 (overlay/kernel-arg schematic inputs — gates the overlay annotation), P3/P5
(provisioned machines never pick up extension changes — makes first-render correctness
permanent), P4 (terraform must stop re-asserting C1-owned labels on terraform-created Hardware).

## Contracts

### Reads

| Object / field | Owner (writer) | Used for |
| --- | --- | --- |
| `Hardware.status.attributes.outOfBand` (`gpuDevices[].vendor/model`, `cpu.sockets[].vendor`, `baseboard`, `product`, `lastUpdated`) | rufio MachineReconciler | trait + extension rules; marker generation |
| `Hardware.spec.interfaces[].dhcp.arch`, `spec.disks`, `spec.bmcRef` | discovery controller (C0) / terraform | arch guard for ucode; refresh-stamp target |
| `Hardware.metadata.annotations["talos.tinkerbell.org/system-extensions"]` | C1 itself (exclusive on Hardware; pre-C1 manual values tolerated) | merge input |
| `Hardware.metadata.annotations["tinkerbell.org/disable-outofband-inventory"]` | operator | inventory-impossible detection |
| `bmc.tinkerbell.org Machine` (existence, annotations) | discovery controller / terraform | refresh stamping |
| `TinkerbellMachineTemplate.spec.template.spec.hardwareAffinity` | user / terraform | coverage validation, convention lint |
| `TalosConfigTemplate` labels; `TalosControlPlane` labels | user | pool coverage advertisement (`covers-*`) |
| `MachineDeployment.spec.template.spec.{bootstrap.configRef,infrastructureRef}` | user | pool → template resolution |

### Writes

All writes use SSA Apply, field manager `talos-hardware-classifier`, asserting only the keys
below (metadata-only apply configurations; C1 never asserts any `spec` or `status` field).

| Object / field | Keys owned (exact) | Notes |
| --- | --- | --- |
| `Hardware` labels | `classify.tinkerbell.org/gpu-vendor`, `classify.tinkerbell.org/cpu-vendor`, `classify.tinkerbell.org/board`, `classify.tinkerbell.org/classified` | trait labels + completion marker |
| `Hardware` labels (optional policy) | `tinkerbell.org/role` | only when role policy enabled and label absent |
| `Hardware` annotations | `talos.tinkerbell.org/system-extensions`, `classify.tinkerbell.org/last-applied-extensions`, `classify.tinkerbell.org/uncovered-traits` | CAPT contract key + merge companion + coverage surface |
| `Hardware` annotations (post-P2, flag-gated) | `talos.tinkerbell.org/overlay`, `talos.tinkerbell.org/extra-kernel-args` | P2 contract keys, consumed by CAPT `pkg/schematic` once P2 lands |
| `bmc.tinkerbell.org Machine` annotations | `tinkerbell.org/refresh-inventory` (foreign contract, write `"true"` only; rufio clears it) | refresh stamping |
| Events | reasons `Classified`, `ReclassifiedStale`, `InventoryTimeout`, `UncoveredTrait`, `PoolMissingClassifiedGate` | on Hardware / templates |

Conditions: `Hardware.status` has no conditions field (`tinkerbell/api/v1alpha1/tinkerbell/hardware.go:421-431`),
so C1 surfaces state via Events plus the `uncovered-traits` annotation and Prometheus metrics —
this is the concrete realization of the charter's "condition/event" requirement.

## Design

### Classification policy model

Classification is a pure function `Classify(hw, policy) → {traits, extensions, overlay, basis}`
over the `Hardware` object. Built-in rules, in evaluation order:

| # | Signal | Trait label | Extensions contributed | Notes |
| --- | --- | --- | --- | --- |
| 1 | `outOfBand.gpuDevices[].vendor` matches NVIDIA (case-insensitive substring `nvidia` or PCI vendor id `10de`) | `gpu-vendor=nvidia` | kernel-module extension + `siderolabs/nvidia-container-toolkit-<branch>` (see branch/flavor decision) | best-effort per F6; manual annotation on `TinkerbellMachine` is the authoritative override |
| 2 | `outOfBand.cpu.sockets[].vendor` contains `Intel` / `AMD`, AND architecture is `amd64` | `cpu-vendor=intel` / `cpu-vendor=amd` | `siderolabs/intel-ucode` / `siderolabs/amd-ucode` | arch derived exactly as CAPT does (`pkg/schematic/schematic.go:85-100`); no ucode extensions exist for arm64 |
| 3 | `spec.disks[].device` has `/dev/nvme*` prefix | none | **none — deliberately** | see Decision below |
| 4 | `outOfBand.baseboard` (fallback `product`) vendor+model | `board=<slug>` (e.g. `asrockrack-x570d4i-2t`, `raspberrypi-5-model-b`; RFC1123-slugified, 63-char truncated) | none today; post-P2 drives the overlay annotation | pool selection only until P2 |

> **Decision — NVMe stays CAPT's.** CAPT's built-in schematic rule already adds
> `siderolabs/nvme-cli` from `Hardware.spec.disks` for every machine, classified or not
> (`pkg/schematic/schematic.go:150-152`). C1 does not duplicate it into the annotation: two
> writers deriving one fact from the same signal creates a retraction hazard (if disks change,
> C1's stale annotation entry would keep the extension alive after CAPT's rule stops matching)
> and buys nothing — CAPT's `Build` deduplicates anyway. Rule of thumb recorded for future
> rules: signals CAPT reads from `Hardware.spec` belong to CAPT's built-ins; signals only
> visible in `status.attributes.outOfBand` belong to C1's annotation.

> **Decision — NVIDIA flavor and branch.** The kernel-module extension is selected by two
> policy knobs: flavor `proprietary` → `siderolabs/nonfree-kmod-nvidia-<branch>`, flavor
> `open` → `siderolabs/nvidia-open-gpu-kernel-modules-<branch>`; branch is `production` or
> `lts`. Defaults: `--nvidia-flavor=proprietary`, `--nvidia-branch=production`. Rationale:
> BMC inventory cannot reveal GPU generation, the proprietary kmod supports every generation
> the open modules do plus pre-Turing cards, and using one branch suffix for both kmod and
> toolkit guarantees the driver-version match the extensions require
> (docs.siderolabs.com Talos v1.13 NVIDIA guides; live Factory extension list verified
> 2026-09-01). Overrides: global flags, plus per-Hardware annotations
> `classify.tinkerbell.org/nvidia-flavor` and `classify.tinkerbell.org/nvidia-branch`.
> `nvidia-fabricmanager-*` (NVSwitch/HGX only) is never auto-selected; operators add it via
> the `TinkerbellMachine`-side annotation.

The policy is versioned in code (a `policyRevision` constant); bumping it triggers
reclassification on the next resync but never moves the classified marker by itself (see marker
semantics).

### Extension merge on `talos.tinkerbell.org/system-extensions`

C1 is the exclusive writer of this annotation on `Hardware` (users are directed to the
`TinkerbellMachine`-side annotation, which CAPT unions in — `controller/machine/schematic.go:44`
plus `pkg/schematic/schematic.go:68-72`). Exclusivity is forward-looking; values that predate C1
must survive. Both properties come from a three-way merge with the companion annotation
`classify.tinkerbell.org/last-applied-extensions`:

```text
base    := parseSet(annotations["classify.tinkerbell.org/last-applied-extensions"])  # ∅ if absent
current := parseSet(annotations["talos.tinkerbell.org/system-extensions"])           # live value
desired := Classify(hw, policy).Extensions                                           # this pass

foreign := current − base                  # entries C1 never wrote: preserve verbatim
merged  := sortedDedupe(foreign ∪ desired)

if merged == sortedDedupe(current) AND sortedSet(desired) == base:
    return  # no-op, no write
apply with SSA (one request, manager "talos-hardware-classifier"):
    annotations["talos.tinkerbell.org/system-extensions"]          = join(merged, ",")
    annotations["classify.tinkerbell.org/last-applied-extensions"] = join(sorted(desired), ",")
    # when merged and desired are both empty, both keys are omitted from the
    # apply configuration → SSA removes them (C1 owns them)
```

Properties: **retraction** — an entry in `base` but not in `desired` is absent from `foreign`
and disappears (e.g. policy branch change swaps `-lts` for `-production` cleanly); **adoption** —
a pre-C1 manual value (`base = ∅`) is preserved as `foreign` forever; **convergence** — a user
deleting an entry C1 desires gets it re-added (by design; the user override channel is the
`TinkerbellMachine` annotation). Values are comma-separated and sorted, matching CAPT's parser
(`controller/machine/schematic.go:116-131`), and both keys are written in the same Apply so the
pair can never be observed torn.

### Classified marker semantics

`classify.tinkerbell.org/classified` is a **label** (not an annotation) because the pool
convention needs it in `hardwareAffinity` label selectors. Its value is the inventory generation
the classification was computed from: `status.attributes.outOfBand.lastUpdated` rendered
label-safe as compact UTC, e.g. `20260901T171233Z` (Go format `20060102T150405Z`; RFC3339 colons
are not legal in label values). Sentinel value `spec-only` marks a classification made without
out-of-band inventory.

Movement rules:

- Stamped after the first classification pass completes and its writes are applied — with the
  real generation when inventory backed it, with `spec-only` otherwise (see Decision below).
- Moves forward (re-stamped to the new timestamp) whenever a reclassification pass runs against
  a strictly newer `lastUpdated`; moves from `spec-only` to a timestamp when inventory first
  arrives. It never moves backward.
- Never removed: not on release (traits stay valid across claims — C4 is forbidden to touch
  classification), not on policy changes, not on controller restart.
- Claims/releases and C1 restarts do not move it; pool selectors only test existence, so the
  value is purely observability plus staleness input for reclassification.

> **Decision — bounded fail-open for inventory-less hardware.** The charter says the marker is
> "stamped only after inventory-backed classification"; taken literally, hardware with no
> `spec.bmcRef`, with `tinkerbell.org/disable-outofband-inventory` set, or with a dead BMC
> would never become claimable — a deadlock, contradicting the charter's own fail-open
> philosophy and F6's "detection is best-effort" reality. Resolution: when out-of-band
> inventory is *structurally impossible* (no `bmcRef`, or the disable annotation), C1 stamps
> `classified=spec-only` immediately after a spec-only pass (there is no staleness window to
> wait out). When inventory is *possible but absent/stale*, C1 stamps the refresh annotation,
> waits up to `--inventory-wait-timeout` (default `10m`, measured from first sight), then
> stamps `spec-only` and emits Event `InventoryTimeout`. The degradation is visible (event +
> metric + the `spec-only` value itself) and the residual risk — a machine imaged during the
> window permanently lacking GPU/ucode extensions (provisioned short-circuit + P3/P5) — is
> exactly the documented limitation, now bounded to 10 minutes instead of forever.

### Inventory refresh stamping

On first sight of a `Hardware` without the classified label whose inventory is possible but
absent or older than `--inventory-max-age` (default `24h`, rufio's own cadence):

1. Resolve `spec.bmcRef` to the `bmc.tinkerbell.org Machine` in the Hardware's namespace.
2. If the Machine does not already carry `tinkerbell.org/refresh-inventory`, SSA-apply it with
   value `"true"` (foreign contract per the naming standard; rufio's MachineReconciler collects
   on its next reconcile and clears the annotation on success —
   `rufio/internal/controller/inventory.go:49,136-156`).
3. Requeue with the wait timeout as deadline. The Hardware watch fires when rufio patches
   `status.attributes`, triggering reclassification; C1 does not poll.
4. Re-stamp at most once per `--inventory-wait-timeout` window to avoid annotation churn against
   a failing BMC.

### Pool convention and coverage validation

The documented pool convention (normative statement in `docs/architecture.md`, restated here
because C1 enforces its observability half):

- Every `TinkerbellMachineTemplate.spec.template.spec.hardwareAffinity.required` term MUST
  include `matchExpressions: [{key: classify.tinkerbell.org/classified, operator: Exists}]`.
  This is what closes the classification race: CAPT's claim path matches labels at claim time
  (`controller/machine/hardware.go:187-227`), so absent marker ⇒ unclaimable ⇒ no schematic is
  ever baked from unclassified hardware.
- A pool whose bootstrap template supplies companion config for a trait advertises it with a
  label on the `TalosConfigTemplate`: key `classify.tinkerbell.org/covers-<trait>` with the
  trait value, e.g. `classify.tinkerbell.org/covers-gpu-vendor: "nvidia"`. The same labels on a
  `TalosControlPlane` advertise cluster-uniform control-plane coverage.

> **Decision — per-trait `covers-*` keys.** The charter sketched a single
> `classify.tinkerbell.org/covers=<trait>` label; a single key cannot advertise two traits
> (label keys are unique per object). The scheme is therefore one key per trait,
> `covers-<trait-name>=<trait-value>`, which also lets coverage checks be exact value matches.

Coverage validation, run on every classification pass and on pool-object changes, for each trait
that requires companion config (today exactly one: `gpu-vendor=nvidia` requires a
`machine.kernel.modules` patch — `nvidia`, `nvidia_uvm`, `nvidia_drm`, `nvidia_modeset`):

1. Enumerate pools: `MachineDeployment`s whose `bootstrap.configRef` kind is
   `TalosConfigTemplate` and `infrastructureRef` kind is `TinkerbellMachineTemplate`, plus the
   control-plane pair (`TalosControlPlane` + its `TinkerbellMachineTemplate`).
2. `selecting` := pools whose `hardwareAffinity` required terms match this Hardware's labels
   (same OR-of-terms semantics CAPT uses).
3. `covered` := any selecting pool whose bootstrap object carries
   `covers-gpu-vendor: nvidia`.
4. If `selecting` is empty or `covered` is false: Event `UncoveredTrait` (Warning) on the
   Hardware and annotation `classify.tinkerbell.org/uncovered-traits: gpu-vendor=nvidia`
   (comma-separated list; key removed when coverage appears). Metric
   `classifier_uncovered_hardware` gauge by trait.
5. Convention lint: a selecting `TinkerbellMachineTemplate` whose affinity lacks the
   `classified Exists` term gets Event `PoolMissingClassifiedGate` (the race is open for that
   pool). C1 warns; it never mutates templates.

### Role assignment (optional, config-driven)

Off by default. When enabled, C1 stamps `tinkerbell.org/role` (terraform's existing pool-selection
key, `modules/cluster/main.tf` hardwareAffinity `matchLabels`) on Hardware that lacks it — never
overwriting an existing value, so terraform- or operator-set roles always win. Configuration
stays flag-shaped per repo convention: repeated `--role-rule=<trait>=<value>:<role>` (first match
wins, evaluated in flag order) with `--default-role=<role>` as fallback; both unset ⇒ feature
disabled. Example: `--role-rule=gpu-vendor=nvidia:gpu-worker --default-role=worker`.

### Reconcile flow (Hardware)

1. Fetch `Hardware`; ignore not-found. No finalizer — C1 has no teardown duties.
2. Determine inventory state: `fresh` (lastUpdated within max-age), `stale`, `absent`,
   `impossible` (no `bmcRef` or disable annotation).
3. If unclassified and state is `stale`/`absent`: stamp refresh on the rufio Machine (once per
   window), requeue at the wait deadline; continue to step 4 only if the deadline has passed.
4. `Classify(hw, policy)` → traits, desired extensions, basis.
5. Compute the extension merge (algorithm above) and the desired label set (traits + marker +
   optional role).
6. Run coverage validation → events + `uncovered-traits` annotation value.
7. Diff against live metadata; if changed, one SSA Apply of the metadata-only apply
   configuration (labels + annotations together — marker, traits, extensions, companion,
   uncovered-traits are never observable torn).
8. Emit `Classified`/`ReclassifiedStale` events on transitions; update metrics.

### Ordering and races addressed

- **Claim-before-classify** (the permanent-wrong-schematic race): closed by the pool convention
  (`classified Exists` gate) — C1's marker is only stamped in the same Apply as the extension
  annotation, so a claimable Hardware always carries its extensions. Residual: pools ignoring
  the convention; surfaced by `PoolMissingClassifiedGate`, accepted as fail-open.
- **Reclassify-after-provision**: new inventory on a provisioned machine updates the annotation,
  but F2's provisioned short-circuit plus P3/P5 make it inert for that machine. C1 still writes
  (the truth is durable for the *next* claimant after release) and emits `ReclassifiedStale`
  noting the machine will not converge until P3/P5 land.
- **Discovery controller resync (C0)**: the current syncer replaces `Hardware.spec` wholesale
  but merges labels/annotations (`internal/sync/syncer.go:84-125`), so C1's metadata-only writes
  survive even before the C0 SSA migration. No dependency on C0's fix.
- **Terraform re-apply** (P4 unmet): terraform re-asserts labels on terraform-created Hardware
  and can revert C1's writes until `ignore_changes` lands; C1 simply re-applies on its next
  pass (level-triggered, idempotent). Documented as visible churn, not corruption.
- **Two C1 replicas**: leader election; single writer.

### Idempotency

Classification is a pure function of (`Hardware`, policy); the merge is deterministic and
sorted; writes happen only on diff; SSA Apply with a fixed manager makes repeats no-ops. The
refresh stamp is guarded by presence-check plus per-window limit. Marker movement is monotonic.

## Worked example — NVIDIA GPU end-to-end

1. A discovered amd64 node's BMC reports a GPU: rufio writes
   `status.attributes.outOfBand.gpuDevices[0].vendor: "NVIDIA Corporation"` (24h cadence, or
   immediately after C1's refresh stamp on the rufio Machine).
2. C1 classifies: labels `classify.tinkerbell.org/gpu-vendor=nvidia`,
   `classify.tinkerbell.org/cpu-vendor=amd` (say), marker
   `classify.tinkerbell.org/classified=20260901T171233Z`; annotation
   `talos.tinkerbell.org/system-extensions: siderolabs/amd-ucode,siderolabs/nonfree-kmod-nvidia-production,siderolabs/nvidia-container-toolkit-production`
   with the same set in `last-applied-extensions`. One SSA Apply.
3. A GPU worker pool exists: `MachineDeployment` → `TinkerbellMachineTemplate` with
   `hardwareAffinity.required` matching `tinkerbell.org/role=gpu-worker` (stamped by C1's role
   policy) plus `classified Exists`; its `TalosConfigTemplate` carries label
   `classify.tinkerbell.org/covers-gpu-vendor: "nvidia"` and supplies the companion config in
   pool-level `strategicPatches`:

   ```yaml
   machine:
     kernel:
       modules:
         - name: nvidia
         - name: nvidia_uvm
         - name: nvidia_drm
         - name: nvidia_modeset
   ```

4. C1's coverage validation finds the selecting pool covered → no `UncoveredTrait` event, no
   `uncovered-traits` annotation.
5. CAPT claims the Hardware (marker present), reads the annotation, unions it with its NVMe
   built-in, registers the schematic, publishes
   `status.{schematicID,installerImage,diskImageURL}` — the Factory image now contains kmod +
   toolkit — and injects the trio into the Workflow `hardwareMap`; CABPT injects the matching
   `machine.install.image`. The node boots with the drivers baked in and the pool patch loads
   the modules.
6. Counter-case: if no covering pool existed, step 4 instead emits `UncoveredTrait` and stamps
   `classify.tinkerbell.org/uncovered-traits: gpu-vendor=nvidia` — the operator sees exactly
   which trait lacks companion config *before* wondering why `nvidia-smi` is absent.

## Failure modes and degradation

| Dependency | When absent / stale / down | Behavior |
| --- | --- | --- |
| rufio inventory | absent or stale | refresh stamp → bounded wait → `spec-only` classification + `InventoryTimeout` event; upgrades to inventory-backed when data arrives |
| BMC unreachable / lies | inventory never/partially arrives | same as above; GPU detection best-effort per F6 — the `TinkerbellMachine` annotation remains the authoritative manual override |
| rufio controller down | refresh annotation never processed | identical to stale-inventory path; annotation persists and is honored when rufio returns |
| CAPT down | annotation unread | no effect on C1; extensions are consumed at next CAPT reconcile |
| Pool objects missing (no MachineDeployments — today's environment) | coverage cannot be evaluated | `UncoveredTrait` fires for companion-requiring traits (nothing selects the Hardware), which is the honest signal; plain traits classify normally |
| C1 down | no classification | new Hardware stays unclaimable under the pool convention (visible: machines pending, no Hardware match) — the same trade-off as C3's hook; existing classifications persist |
| API conflicts / discovery resync / terraform re-apply | metadata churn | level-triggered re-apply; C1's keys survive the current syncer's label/annotation merge |

Nothing in C1 fail-closes provisioning by itself; the only blocking effect is the pool
convention's `classified Exists` gate, which is bounded by the 10-minute inventory wait.

## Limitations (stated honestly)

- Out-of-band GPU/CPU detection is best-effort on this fleet: Pi-class OpenBMC and ASRock Rack
  BMCs may report no `gpuDevices` at all (F6, MEMORY.md). The manual
  `talos.tinkerbell.org/system-extensions` annotation on `TinkerbellMachine` is the
  authoritative override and is never touched by C1.
- Control-plane platform overrides are cluster-uniform: one `TalosControlPlane` = one
  `strategicPatches` set for all CP nodes; there are no per-trait CP sub-pools. A GPU on one CP
  node means the modules patch applies to every CP node or none.
- The extensions annotation cannot express Image Factory overlays or kernel args until P2 lands
  in the CAPT fork (`pkg/schematic` models `systemExtensions` only,
  `pkg/schematic/schematic.go:124-131`); until then the `board` trait selects pools but cannot
  influence the image, and arm64 SBC bootability rests on P2 regardless of C1.
- Extension changes for already-provisioned machines are inert until P3/P5 land (provisioned
  short-circuit, installer-image drift excluded from rollout comparison). C1 keeps the truth
  current for the next provision and says so in events.

## RBAC

This component requires a `ClusterRole` (the repo's precedent is namespace-scoped Roles — F9;
CAPI objects live in cluster namespaces, Tinkerbell objects in the tinkerbell namespace):

| API group | Resources | Verbs |
| --- | --- | --- |
| `tinkerbell.org` | `hardware` | `get,list,watch,patch` (SSA Apply = `patch`) |
| `bmc.tinkerbell.org` | `machines` | `get,list,watch,patch` |
| `cluster.x-k8s.io` | `machinedeployments` | `get,list,watch` |
| `bootstrap.cluster.x-k8s.io` | `talosconfigtemplates` | `get,list,watch` |
| `controlplane.cluster.x-k8s.io` | `taloscontrolplanes` | `get,list,watch` |
| `infrastructure.cluster.x-k8s.io` | `tinkerbellmachinetemplates` | `get,list,watch` |
| `""` (core) | `events` | `create,patch` |
| `coordination.k8s.io` | `leases` (namespace Role) | `get,create,update` |

## Deployment

Chart `helm/talos-hardware-classifier` following the repo pattern (Deployment + SA + RBAC +
values→flags; research/target-repo.json conventions): 1 replica, no `hostNetwork`, default
rolling strategy (no host-port pinning, unlike discovery), leader election ID
`talos-hardware-classifier.classify.tinkerbell.org`, metrics `:8080`, probes `:8081`, slog
factory with `logger=classifier`. **No Service, no TLS, no cert-manager** — C1 has no webhook.
Image `ghcr.io/tinkerbell-community/talos-hardware-classifier` (goreleaser `builds[]` +
`dockers_v2[]` entries added to the existing `.goreleaser.yaml`, scratch base, user 65532).

Flags (camelCase helm values map 1:1): `--namespace` (Tinkerbell resources; empty = all),
`--inventory-wait-timeout=10m`, `--inventory-max-age=24h`, `--nvidia-flavor=proprietary`,
`--nvidia-branch=production`, `--role-rule` (repeatable), `--default-role`,
`--enable-overlay-annotations=false` (post-P2), `--leader-elect`, `--metrics-bind-address`,
`--health-probe-bind-address`, `--log-level`, `--log-format`.

## Implementation plan

Packages (single module, per naming standard):

- `cmd/talos-hardware-classifier/main.go` — flags, scheme (`clientgoscheme`, `tinkv1`, `bmcv1`,
  `clusterv1`, `bootstrapv1`, `controlplanev1`, `infrav1` — the CAPT `api/` module is standalone
  and importable), manager, controller wiring.
- `internal/classify/` — pure logic, no client:
  - `types.go`: `type Traits map[string]string`, `type Basis int`
    (`BasisInventory`, `BasisSpecOnly`), `type Classification struct { Traits Traits; Extensions []string; Overlay string; Basis Basis; Generation string }`.
  - `rules.go`: `func Classify(hw *tinkv1.Hardware, pol Policy) Classification`,
    `func architectureOf(hw *tinkv1.Hardware) string` (mirrors CAPT), `func boardSlug(a *tinkv1.Attributes) string`.
  - `merge.go`: `func MergeExtensions(current, lastApplied, desired []string) (merged, companion []string, changed bool)`.
  - `policy.go`: `type Policy struct { NvidiaFlavor, NvidiaBranch string; RoleRules []RoleRule; DefaultRole string; OverlayEnabled bool }`, flag parsing helpers.
- `internal/classify/controller.go` — `type HardwareReconciler struct { client.Client; Recorder record.EventRecorder; Policy classify.Policy; Clock clock.Clock; ... }` with
  `func (r *HardwareReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)` and
  `func (r *HardwareReconciler) SetupWithManager(mgr ctrl.Manager) error`.
- `internal/classify/coverage.go` — `func EvaluateCoverage(ctx context.Context, c client.Reader, hw *tinkv1.Hardware, traits classify.Traits) (uncovered []string, lint []PoolLint, err error)`.

Controller-runtime wiring:

- `For(&tinkv1.Hardware{})` with a predicate ignoring updates that touch only C1-owned metadata
  keys (self-echo suppression) while passing spec, `status.attributes`, and foreign-label
  changes.
- `Watches(&bmcv1.Machine{}, handler.EnqueueRequestsFromMapFunc(machineToHardware))` backed by a
  field index on `Hardware` `spec.bmcRef.name` (same index rufio uses).
- `Watches` on `TalosConfigTemplate`, `TinkerbellMachineTemplate`, `MachineDeployment`,
  `TalosControlPlane` mapping to all classified Hardware (coarse, rate-limited via a shared
  `workqueue` token — pool changes are rare; correctness over precision).
- SSA writes via `client.Apply` with typed apply configurations restricted to
  `metadata.labels`/`metadata.annotations`, `client.FieldOwner("talos-hardware-classifier")`,
  `client.ForceOwnership` (C1 is the chartered owner of its keys).

Milestones:

- **M1** — rules + merge + marker: `internal/classify` pure functions, Hardware reconciler with
  SSA apply, `spec-only` fallback for impossible inventory, events/metrics. Deployable and
  useful alone (traits + extensions).
- **M2** — refresh stamping and bounded wait: bmcRef index, rufio Machine annotation flow,
  `InventoryTimeout` path.
- **M3** — pool convention surfaces: coverage validation, `covers-*` resolution through
  MachineDeployment/TalosControlPlane, `PoolMissingClassifiedGate` lint, role policy.
- **M4** — post-P2 overlay/kernel-args annotations behind `--enable-overlay-annotations`, with
  the board→overlay table (`raspberrypi-5*` → `rpi_5`, other `raspberrypi*` → `rpi_generic`,
  extensible via flag `--board-overlay=<board-slug>:<overlay>`).

Test strategy: table-driven unit tests for `rules.go` (per-vendor fixtures incl. empty/partial
OpenBMC-style inventories) and `merge.go` (adoption, retraction, foreign preservation, empty-set
removal); envtest with the Tinkerbell v1alpha1 CRDs (from `tinkerbell/crd/bases`) plus CAPI/CAPT
CRDs for coverage tests — asserting SSA managed-fields ownership (a second manager's keys
survive C1's Apply), marker monotonicity, refresh-stamp single-shot per window, and the
claim-race scenario (Hardware labeled by a fake claimer mid-reconcile; C1's Apply must not
disturb owner labels). No Talos API or Factory access exists in C1, so no fake-Talos-API server
is needed — the merge contract with CAPT is covered by a golden test that round-trips the
annotation through a vendored copy of CAPT's parser semantics.

## Non-goals

- No schematic computation, no Image Factory calls, no knowledge of Talos versions (F2 — CAPT
  owns resolution; C1 supplies inputs only).
- No writes to `Hardware.spec` (userData, os metadata, netboot, disks) — C2/C4/C0 territory.
- No `TalosConfig`/`TalosConfigTemplate`/`TalosControlPlane` mutation: per-machine config
  mutation is architecturally excluded (webhook immutability; CACPPT verbatim stamping), and
  companion config lives in pool-level `strategicPatches` by convention.
- No admission webhook, no Runtime SDK hook, no TLS (F1, F8).
- No in-band inventory collection (future `attributes.inBand` is additive; a rule source, not a
  C1 collector).

## Open questions

- Should `spec-only` classifications re-arm the refresh stamp periodically (e.g. daily) in case
  a BMC starts answering, or is rufio's own 24h cadence sufficient? Current answer: rely on
  rufio's cadence; revisit if fleet BMCs prove flappy.
- When `attributes.inBand` lands upstream, should in-band PCI data outrank out-of-band GPU data
  (it is strictly richer), and does the marker generation become
  `max(outOfBand.lastUpdated, inBand.lastUpdated)`? Deferred until the field exists.
- Whether the coverage validator should also verify the *content* of pool `strategicPatches`
  (parse for `machine.kernel.modules`) rather than trusting the `covers-*` label. Deliberately
  label-only for now: parsing user patches couples C1 to config schemas it must not own.
