# Runtime Hook Glue - Talos + Tinkerbell

This repo is a collection of independently deployable lifecycle glue components that bridge every gap
between the Tinkerbell CAPI infrastructure provider (CAPT) and Talos Linux node lifecycle, for CAPI
clusters provisioned by the terraform at `/home/appkins/src/tfc/cluster-bootstrap`. It also still hosts
the original mDNS BMC discovery controller, which stays as-is apart from the C0 field-ownership fix.

The full design lives under `docs/`. Read [docs/architecture.md](docs/architecture.md) first — it is the
binding system contract (governing facts, ownership matrix, preconditions, naming standard). Each
component has an implementation-grade design doc listed below. Design date: 2026-09-01.

## The mechanism reality (read before assuming Runtime SDK hooks)

The original brief assumed CAPI Runtime SDK lifecycle hooks and topology mutation hooks. **Those never
fire here**: the workload cluster is created without ClusterClass (`spec.topology` unset), and every
lifecycle/mutation hook is invoked only by the topology controller (cluster-api
`internal/controllers/topology/cluster/cluster_controller.go:115,280`; upstream issue #11491). The
in-place update hook surface (`CanUpdateMachine`/`UpdateMachine`) is exclusively owned by the CABPT fork
— CAPI hard-fails a second `UpdateMachine` extension. Therefore this repo registers **zero** Runtime SDK
hooks and needs no ExtensionConfig machinery. The mechanisms actually used:

- **Machine deletion phase hook annotations** (`pre-terminate.delete.hook.machine.cluster.x-k8s.io/<name>`)
  — core Machine controller, no feature gates, work for every Machine (C3).
- **Plain controllers** watching CAPI + Tinkerbell objects (C0, C1, C2, C4, C5).
- **Admission webhooks** on Tinkerbell CRDs for ordering guarantees (C2, opt-in).

If ClusterClass is ever adopted, revisit the Runtime SDK path — see Future work in architecture.md.

## Components

Each component: own binary (`cmd/<name>`), own Helm chart (`helm/<name>`), own RBAC, one goreleaser
build + image. Components interact only through the API server via contracts in the ownership matrix
(one writer per field per state; SSA field manager == component name, with recorded exceptions).

### C0 — discovery controller field ownership ([docs/discovery-field-ownership.md](docs/discovery-field-ownership.md))

Migrates the existing discovery controller's `CreateOrUpdate` full-spec rewrites to sparse server-side
apply (field manager `tinkerbell-bmc-discovery-controller`). Today `internal/sync/syncer.go:84-124`
rebuilds `Hardware.spec` from scratch every resync (1h), wiping CAPT's `spec.userData` (bricks installs,
breaks tootles for running nodes), C2's `operating_system` metadata, and the tink workflow controller's
`allowPXE=false` disarm — an active corruption bug and a hard prerequisite for C2/C4. Creates via POST,
updates via sparse SSA asserting only discovery-owned fields, carries live netboot values forward
(atomic `interfaces` list), never serializes foreign fields. This doc is the repo-wide SSA discipline
reference. **Build first.**

### C1 — talos-hardware-classifier ([docs/talos-hardware-classifier.md](docs/talos-hardware-classifier.md))

Turns rufio's out-of-band inventory (`Hardware.status.attributes.outOfBand`) into trait labels
(`classify.tinkerbell.org/gpu-vendor|cpu-vendor|board`) and the Hardware-side
`talos.tinkerbell.org/system-extensions` annotation that the CAPT fork unions into Image Factory
schematics (NVIDIA GPU → kmod + container-toolkit pair; CPU vendor → ucode). A three-way merge via a
last-applied companion annotation lets C1 retract its own entries without stomping user entries; user
overrides go on the TinkerbellMachine-side annotation. The `classify.tinkerbell.org/classified` **label**
(value: inventory generation, or `spec-only` after a bounded wait) gates claiming through the pool
convention — `hardwareAffinity` must require the marker so unclassified hardware is never claimed and
imaged with a wrong schematic (permanent: CAPT never re-images provisioned machines). Coverage-validation
events flag classified hardware no pool's bootstrap template covers (GPU extensions without the
`machine.kernel.modules` companion patch are a silent failure). Detection is best-effort on this fleet's
BMCs; the manual annotation is authoritative. Metadata-only writes; never touches `Hardware.spec`.

### C2 — talos-os-metadata ([docs/talos-os-metadata.md](docs/talos-os-metadata.md))

Mirror controller plus an **opt-in** validating webhook. Mirrors CAPT's resolved status trio
(`schematicID`/`installerImage`/`diskImageURL`) into `Hardware.spec.metadata.instance.operating_system`
for claimed Hardware (all values derived from `status.diskImageURL`, so the mirror cannot disagree with
the resolver) — feeding the workflow template's `IMG_URL` and tootles' EC2 `operating-system` endpoints.
The webhook (disabled by default; the brief's "blocking hook", realized as admission because no CAPI hook
can fire) gates `workflows.tinkerbell.org` CREATE, scoped by objectSelector to CAPT-labeled Workflows
only (auto-enrollment never blocked): mirror stale → retriable deny (CAPT backoff-retries); status trio
empty → **admit** with surfaced events (fail-open on the data axis — a Factory outage or missing
talosVersion must degrade visibly, never deadlock provisioning). cert-manager provides the repo's first
TLS. Never clears on release — that is C4's job (state-disjoint handoff).

### C3 — talos-machine-teardown ([docs/talos-machine-teardown.md](docs/talos-machine-teardown.md))

The destructive-path fix: today machine deletion is power-off only (CAPT deletes Template/Workflow CRs,
runs one rufio `PowerHardOff` job, releases Hardware — no etcd leave, no reset, no wipe; CACPPT leaves
etcd only on its own scale-down). C3 stamps
`pre-terminate.delete.hook.machine.cluster.x-k8s.io/talos-teardown` on Machines backed by
TalosConfig + TinkerbellMachine at earliest reconcile, then acts when the Machine reaches the
pre-terminate phase (strictly before CAPT's power-off; runs last after other hooks, KCP-style). Control
plane: **verify-then-act** etcd removal — list members via a healthy peer; still a member → forfeit +
leave via the victim, or `EtcdRemoveMemberByID` via the peer when dead (CACPPT's `etcd-leaving`
annotation is a hint, never proof: it is stamped before the leave and failed leaves are swallowed by
`scale.go:163-165`). Then Talos `Reset` (graceful=false, halt, wipe STATE+EPHEMERAL) with bounded
deadlines, fail-open for dead nodes; caches the talosconfig at stamp time to survive cluster-delete
secret GC. Covers scale-down, `kubectl delete`, MHC remediation, workers, whole-cluster delete.

### C4 — tinkerbell-hardware-janitor ([docs/tinkerbell-hardware-janitor.md](docs/tinkerbell-hardware-janitor.md))

Hygiene for released Hardware, including force-delete paths where C3 never ran. Released predicate
(three conjuncts, each load-bearing): owner labels absent ∧ `spec.userData` non-empty ∧ provisioned
annotation absent — the latter two keep it away from never-claimed hardware and the terraform bootstrap
node. One resourceVersion-guarded Update (never blind merge patch — defends the release-clear vs
re-claim race) clears `spec.userData` (the full secret-bearing machineconfig that tootles otherwise
serves unauthenticated by IP forever), clears `operating_system`/instance state, parks `allowPXE=false`.
Self-extinguishing; never acts on claimed hardware; no BMC/power actions.

### C5 — talos-upgrade-coordinator ([docs/talos-upgrade-coordinator.md](docs/talos-upgrade-coordinator.md))

The cluster-scoped remainder of `talosctl upgrade-k8s`: SSA sync of Talos bootstrap manifests into the
workload cluster with pruning — and nothing else (kube-proxy is a per-node machine-config patch =
CABPT's domain, and moot on this Cilium cluster; per-node kubelet/static-pod versions ride the in-place
pipeline). Gates on real convergence: all Machines UpToDate AND every workload Node at the target minor
(the terraform bootstrap node is invisible to CAPI — Node scan covers it). Applies via
`siderolabs/go-kubernetes` using talosctl's own field manager `talos` (foreign-contract exception) so C5
and manual `talosctl upgrade-k8s` runs share ownership and pruning inventory. Checkpointed by
`upgrade.tinkerbell.org/last-synced-version`; read-only toward CAPI otherwise. Natural future fold-in
candidate for CACPPT.

## What this repo must NOT do

- Register any Runtime SDK hook (in-place hooks are CABPT's; CAPI allows exactly one `UpdateMachine`
  extension — a second registration breaks in-place updates cluster-wide).
- Resolve Image Factory schematics (the CAPT fork owns resolution; C1 supplies inputs, C2 mirrors outputs).
- Generate or apply Talos machine config, or drive `ApplyConfiguration`/`Upgrade` on managed machines
  (CABPT/CACPPT own the whole in-place pipeline).
- Delete Node objects (CAPI machine controller and CACPPT own that).
- Adopt the terraform bootstrap node into CAPI, or source fleet inventory (discovery/terraform own it).

## Preconditions and required cross-repo changes (P1-P7)

The image pipeline is **inert or deficient today** without these; components degrade visibly (events/
conditions), never deadlock, while unmet. Full dependency chains in architecture.md.

- **P1 (terraform, required)**: pass a pinned full `talos_version` through to
  `TalosControlPlane.spec.controlPlaneConfig.controlplane.talosVersion`. Without it CAPT skips schematic
  resolution entirely and the status trio never exists.
- **P2 (CAPT fork, required before C2 replaces terraform-written os metadata)**: schematic inputs for
  `extraKernelArgs` (`talos.config=<tootles URL>`, console) and overlay/bootloader (arm64 `sd-boot`,
  SBC overlays) — today's CAPT schematic produces a raw image that cannot fetch its config.
- **P3 (CAPT fork, required for OS upgrades)**: run schematic resolution for provisioned machines too —
  the provisioned short-circuit freezes `status.installerImage`, so in-place Talos upgrades never fire.
- **P4 (terraform, required)**: `ignore_changes` on component-owned Hardware fields; one Hardware
  creator per physical machine (duplicates break smee/tootles `FilterHardware`).
- **P5 (recommended)**: CACPPT `scale.go:163-165` swallowed `leaveErr` fix; installer-image drift
  propagation (extension-set changes are inert for provisioned machines today); CAPT `DISK_ID`
  substitution from `Hardware.spec.disks[0]`.
- **P6 (invariant)**: provider-id three-way agreement — hardware namespace `tinkerbell`, hostname ==
  hardware name (CABPT `hostname.source=InfrastructureName`), or CCM/CSR breakage is silent.
- **P7**: the workflow template consumes exactly one image field — `{{ .diskImageURL }}` from the
  workflow `hardwareMap` (after P2); compression pinned per Talos minor.

## Naming

- Component = binary = chart = image name, kebab-case: `talos-hardware-classifier`, `talos-os-metadata`,
  `talos-machine-teardown`, `tinkerbell-hardware-janitor`, `talos-upgrade-coordinator`. Prefix `talos-`
  when the subject is Talos lifecycle, `tinkerbell-` when managing Tinkerbell resources per se.
- Layout per component: `cmd/<name>/main.go`, `internal/<shortname>/`, `helm/<name>/`, one goreleaser
  build + image `ghcr.io/tinkerbell-community/<name>`.
- Label/annotation domain per component: `classify.` / `osmeta.` / `teardown.` / `janitor.` /
  `upgrade.tinkerbell.org` — except when fulfilling foreign contracts
  (`talos.tinkerbell.org/system-extensions` is CAPT's; `tinkerbell.org/refresh-inventory` is rufio's;
  the pre-terminate hook key is CAPI's, suffix `talos-teardown`, kept stable across renames).
- SSA field manager identity == component name, except foreign SSA contracts (C5 uses `talos` on
  workload manifests, shared with talosctl).
- Webhook objects: `<name>-webhook`, Service `<name>`, cert-manager Certificate/secret
  `<name>-serving-cert`. Leader election ID `<binary>.<domain>`; kebab-case flags ↔ camelCase Helm
  values; image tags without `v` prefix; chart version independent of appVersion.

## Implementation order

1. **C0** — stops active corruption; prerequisite for C2/C4. Plus P1 (a terraform one-liner) to bring
   the image pipeline to life.
2. **C3** — the teardown gap is the most dangerous (etcd members orphaned, disks left dirty, secrets
   left served); no preconditions.
3. **C4** — small, closes the secret-leak half of teardown; needs C0.
4. **C1** — extension selection + pool convention; useful before P2 lands.
5. **C2** — mirror first (M1); enable the webhook (M2) once P1/P2 hold.
6. **C5** — after upgrade flows are exercised end-to-end.

Cross-repo work (P2, P3, P5) proceeds in the CAPT/CACPPT forks in parallel — this repo's docs are the
spec for those changes.

## Conventions for working here

- Go module `github.com/tinkerbell-community/tinkerbell-bmc-discovery-controller`; strict golangci
  config; slog via `internal/logging` (per-component `logger` attribute); scratch images, cosign,
  goreleaser multi-build; CGO only in tests (`-race`).
- Every new writer follows the C0 SSA discipline (sparse apply, owned fields only, named manager).
- Docs are markdownlint-clean; design changes update the component doc and, if contracts move,
  architecture.md's ownership matrix in the same change.
