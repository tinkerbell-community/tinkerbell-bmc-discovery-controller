# talos-upgrade-coordinator (C5)

Status: Proposed — 2026-09-01

talos-upgrade-coordinator is a management-cluster controller that performs the one piece of
`talosctl upgrade-k8s` that no deployed component owns: server-side-apply synchronization of the
Talos bootstrap manifests into the workload cluster (with Talos v1.13 resource pruning), gated on
actual cluster-wide version convergence. It watches `TalosControlPlane` and CAPI `Machine` objects
for Kubernetes/Talos version transitions, waits until every CAPI Machine is `UpToDate` and every
workload Node — including the terraform-managed bootstrap node that CAPI cannot see — runs the
target minor, then fetches the rendered bootstrap manifests from a control-plane node's Talos COSI
state and applies them to the workload cluster using the same SSA field manager and inventory that
`talosctl` uses. It is read-only toward CAPI apart from a single checkpoint annotation, triggers no
machine-level action of any kind, and is built on `siderolabs/go-kubernetes` plus the Talos
machinery module — deliberately not the full `siderolabs/talos` module.

## Purpose and scope

`talosctl upgrade-k8s` (talos@v1.13.5 `pkg/cluster/kubernetes/talos_managed.go:81`, `Upgrade`) does
five things: detect the lowest running version, pre-pull images, patch control-plane static-pod
images per node, patch the kube-proxy image per node, patch `machine.kubelet.image` per node — and
finally sync the Talos bootstrap manifests into Kubernetes via SSA (`PerformManifestsSync`,
`talos_managed.go:464`), pruning removed resources on v1.13+. In this environment, everything
except the final step is already owned:

- **Per-node static pods and kubelet** are covered by the in-place pipeline: CACPPT copies
  `TalosControlPlane.spec.version` into `Machine.spec.version` (CACPPT
  `controllers/taloscontrolplane_controller.go:479`), CABPT regenerates each machine's full config
  for that Kubernetes version (CABPT `controllers/talosconfig_controller.go:554-585`), and CABPT's
  `UpdateMachine` extension applies the regenerated config via `ApplyConfiguration` in AUTO mode
  (CABPT `internal/inplace/updatemachine.go:41-149`, `internal/inplace/talos.go:102-104`). Talos
  re-renders static pods from machine config. CACPPT orders control-plane machines one at a time,
  etcd-health-gated (CACPPT `controllers/inplace.go:60-110`). Governing fact F3 and precondition P3
  (docs/architecture.md) qualify the Talos-OS half of this pipeline; the Kubernetes-version half is
  live today (F10).
- **kube-proxy is explicitly out of C5's scope**, for two independent reasons. First, the
  config-ownership boundary: `upgradeKubeProxy` is not a workload-cluster write — it is a per-node
  **machine-config** patch. talos@v1.13.5 `pkg/cluster/kubernetes/talos_managed.go:230-243` loops
  control-plane nodes calling `patchNodeConfig`, whose patch function mutates
  `ClusterConfig.ProxyConfig.ContainerImage` (`talos_managed.go:244-260`) and writes it back with
  `ApplyConfiguration` mode `NO_REBOOT` (`patch.go:21-48`). Machine config generation and apply are
  exclusively CABPT's domain (F3): a C5-written config patch would drift from the config CABPT
  renders into `<owner>-bootstrap-data`, be invisible to CABPT's in-place config hash, and be
  silently reverted by the next full-config in-place apply. In a cluster that runs kube-proxy, the
  correct propagation is CABPT config regeneration (the generated config carries the
  version-matched proxy image), after which Talos re-renders the kube-proxy DaemonSet as a
  bootstrap manifest — which C5's manifest sync then delivers. C5 gets kube-proxy right precisely
  by *not* touching it. Second, it is moot here: the deployed cluster disables the proxy and runs
  Cilium `kubeProxyReplacement` (tfc/cluster-bootstrap
  `modules/data-lookup/modules/config-patches/locals.tf:17-171`,
  `modules/core/modules/cilium/locals.tf:1-143`).
- **Pre-pull** is a per-node Talos API action (machine-level; forbidden by the charter's Must-NOT)
  and an optimization, not a correctness requirement. Not C5's job.

What remains — and what C5 is — is the **cluster-scoped remainder**: fetch the bootstrap manifests
that Talos has rendered from the (already-updated) machine configs and server-side-apply them to
the workload cluster, pruning objects Talos no longer renders (F7). Without it, objects like the
Talos-managed RBAC, CoreDNS-adjacent resources, PSP/flannel-era leftovers, and any
machine-config-derived manifest set stay frozen at bootstrap-time versions forever, because
machined applies bootstrap manifests only at cluster bootstrap.

## Context

Per docs/architecture.md: F3 (in-place hooks are CABPT's; C5 never drives
`ApplyConfiguration`/`Upgrade`), F7 (the cluster-scoped remainder of `upgrade-k8s`), F10 (in-place
feature gates are live), and precondition P1 (terraform must pin `talosVersion`; C5 functions
without it but reports the observed Talos version instead) apply. The terraform bootstrap node is
permanently outside CAPI (`replicas = n-1`, provisioned-annotation pre-set; tfc/cluster-bootstrap
`modules/cluster/main.tf:137-141,177-178`), which drives the convergence-gate design below. C5 is
flagged as a natural long-term fold-in candidate for CACPPT; it is kept separate now for
independent deployability and a separate failure domain (see Non-goals).

## Contracts

### Reads

| Object | Field(s) | Owner |
| --- | --- | --- |
| `TalosControlPlane` (`controlplane.cluster.x-k8s.io/v1beta1`) | `spec.version`, `spec.controlPlaneConfig.controlplane.talosVersion`, `metadata.annotations` (`upgrade.tinkerbell.org/*`), labels/ownerRefs for cluster resolution | terraform (spec), CACPPT (status) |
| `Cluster` (`cluster.x-k8s.io/v1beta2`) | `spec.controlPlaneRef` (reverse mapping only) | terraform |
| `Machine` (`cluster.x-k8s.io/v1beta2`) | `status.conditions["UpToDate"]`, `spec.version`, `status.addresses`, label `cluster.x-k8s.io/cluster-name` | core CAPI Machine controller, CACPPT |
| Secret `<cluster>-kubeconfig`, key `value` | workload kubeconfig (type `cluster.x-k8s.io/secret`; seeded by the terraform base chart, tfc/cluster-bootstrap `modules/core/modules/base/chart/templates/tinkerbell.yaml:19-25`) | terraform / CAPI convention |
| Secret `<cluster>-talosconfig`, key `talosconfig` | Talos client config, endpoints kept fresh by CABPT (CABPT `controllers/secrets.go:264-306`) | CABPT |
| Workload `Node` | `status.nodeInfo.kubeletVersion` | kubelet |
| Workload `Pod` (kube-system, label `k8s-app` in `{kube-apiserver, kube-controller-manager, kube-scheduler}`) | `spec.containers[].image` | Talos static pods |
| Talos COSI `k8s.Manifest` resources (namespace `controlplane`) via one CP node | rendered bootstrap manifests | Talos machined |

### Writes

| Object | Field | SSA field manager |
| --- | --- | --- |
| `TalosControlPlane` | `metadata.annotations["upgrade.tinkerbell.org/last-synced-version"]`; removal of `upgrade.tinkerbell.org/force-sync` after honoring it | `talos-upgrade-coordinator` |
| Workload bootstrap-manifest objects (whatever Talos renders: ClusterRoles, DaemonSets, ConfigMaps, …) | whole objects via SSA apply + prune | `talos` (`constants.KubernetesFieldManagerName`, machinery@v1.13.0 `constants/constants.go:1129` — foreign contract, see Decision below) |
| Workload ConfigMap `kube-system/talos-bootstrap-manifests-inventory` | SSA inventory bookkeeping (written by `ssa.Manager`) | `talos` |
| `Event` objects on `TalosControlPlane` | reasons listed under Failure modes | n/a |

Annotations owned by C5 (domain `upgrade.tinkerbell.org` per the naming standard):

- `upgrade.tinkerbell.org/last-synced-version` — written by C5 only, on `TalosControlPlane`. Value
  format `k8s=<version>,talos=<version>`, e.g. `k8s=v1.36.4,talos=v1.13.9`.
- `upgrade.tinkerbell.org/force-sync` — written by a human (`"true"`), consumed and removed by C5:
  one-shot gate bypass (see Failure modes).

C5 owns no conditions (it has no API object of its own and never writes `TalosControlPlane.status`,
which CACPPT owns). Degradation is surfaced via Events and metrics.

> **Decision — workload field manager is `talos`, not the component name.** The naming standard
> mandates field manager == component name *except when fulfilling foreign contracts*. The SSA
> inventory and field ownership of bootstrap manifests are a pre-existing contract shared with
> `talosctl upgrade-k8s` (talos@v1.13.5 `talos_managed.go:530-536` passes
> `constants.KubernetesFieldManagerName`/`KubernetesInventoryNamespace`/`KubernetesBootstrapManifestsInventoryName`).
> Using the same manager and inventory means a human running `talosctl upgrade-k8s` and C5 converge
> on identical ownership instead of fighting over every field of every manifest, and pruning works
> across both actors. Management-cluster writes (the annotation) use `talos-upgrade-coordinator`.

## Design

### Mechanism

One controller-runtime `Reconciler` keyed on `TalosControlPlane`. Level-triggered: each reconcile
computes the *desired pair* (target Kubernetes version, observed Talos version), compares it with
the `last-synced-version` annotation, and if they differ evaluates the convergence gate; only when
the gate passes does it fetch manifests over the Talos API and apply them to the workload cluster.
The annotation makes the sync once-per-version-change and idempotent on retry: it is written only
after a fully successful apply, so any failure leaves the pair unequal and the sync re-runs.

### Trigger model

- **Primary watch**: `TalosControlPlane` (as `unstructured.Unstructured` with GVK
  `controlplane.cluster.x-k8s.io/v1beta1 TalosControlPlane`), predicate: generation change or
  change to `upgrade.tinkerbell.org/*` annotations. A `spec.version` bump (terraform apply) fires
  here.
- **Secondary watch**: `Machine` (`cluster.x-k8s.io/v1beta2`), mapped to the owning
  `TalosControlPlane` via label `cluster.x-k8s.io/cluster-name` → `Cluster.spec.controlPlaneRef`;
  predicate: transition of the `UpToDate` condition or `spec.version` change. This is what wakes C5
  when the in-place rollout finishes (including after a Talos OS upgrade post-P3 — the observed
  Talos version changes the desired pair even though `TalosControlPlane.spec` did not).
- **Poll while blocked**: workload Nodes cannot be watched from the management cluster, and the
  bootstrap node produces no CAPI events at all — so while the pair is unsynced and the gate is not
  yet passed, C5 requeues at `--gate-requeue-interval` (default `1m`). This is how the bootstrap
  node's convergence (terraform-driven) is eventually observed.
- **Optional periodic resync**: `--resync-period` (default `0` = disabled) re-runs the sync at the
  current pair to repair drift. Off by default: SSA with the `talos` manager re-asserting
  Talos-owned fields is exactly what `talosctl` would do, but unattended periodic writes to the
  workload cluster should be an explicit operator choice.

### Convergence gate (exact definition)

The gate deliberately does **not** trust CAPI Machine conditions alone: the terraform bootstrap
node is invisible to CAPI (`replicas = n-1`; tfc/cluster-bootstrap
`modules/cluster/main.tf:137-141`), so "all Machines UpToDate" can be true while one control-plane
node still runs the old kubelet and static pods. Syncing new-minor bootstrap manifests against a
cluster with an old-minor API server risks applying resources the lagging components mishandle,
and permanently hides the lag. The gate therefore combines CAPI state with a live workload scan,
in the style of `DetectLowestVersion` (talos@v1.13.5 `pkg/cluster/kubernetes/detect.go:20-76`).

Let `target` = `TalosControlPlane.spec.version` (e.g. `v1.36.4`). The gate passes iff all of:

- **G1 — Machines**: every `Machine` labeled `cluster.x-k8s.io/cluster-name=<cluster>` in the
  `TalosControlPlane` namespace has condition `UpToDate == True` (cluster-api@v1.13.0
  `api/core/v1beta2/machine_types.go:158`). Fallback: if a Machine carries no `UpToDate` condition
  at all (owner not participating in v1beta2 conditions), it passes G1 iff its `spec.version` minor
  equals the target minor; an event notes the fallback was used.
- **G2 — Nodes**: every workload `Node`'s `status.nodeInfo.kubeletVersion` has minor == target
  minor. This is the clause that covers the bootstrap node's kubelet. Node names listed in
  `--gate-ignore-nodes` are skipped (escape hatch for a dead, undeletable Node object).
- **G3 — control-plane static pods**: a `DetectLowestVersion`-style scan of `kube-system` Pods
  labeled `k8s-app` in `{kube-apiserver, kube-controller-manager, kube-scheduler}` (image-tag
  semver, lowest wins; kube-proxy intentionally omitted — none exists here) finds at least one
  `kube-apiserver` pod and the lowest version's minor == target minor. This covers the bootstrap
  node's static pods, which G1 and G2 cannot see.

Minor-equality (not exact patch equality) is deliberate for G2/G3: the terraform-applied bootstrap
node and CAPI machines may legitimately skew by a patch release for days, and bootstrap manifests
are minor-scoped; requiring exact patch equality would deadlock the sync for no correctness gain.
G1 stays exact because `UpToDate` already encodes CACPPT's own definition of convergence.

### Reconcile flow

1. Fetch the `TalosControlPlane`; if `deletionTimestamp` is set, return (no finalizer — C5 keeps no
   state that needs cleanup; a torn-down cluster simply stops being synced).
2. Resolve the cluster name from the owner `Cluster` reference (CACPPT's own pattern,
   CACPPT `controllers/configs.go:32-58`).
3. Build the workload `*rest.Config` from Secret `<cluster>-kubeconfig` key `value`
   (`clientcmd.RESTConfigFromKubeConfig`); build the Talos client from Secret
   `<cluster>-talosconfig` key `talosconfig` via `config.FromBytes` +
   `client.New(ctx, client.WithDefaultGRPCDialOptions(), client.WithConfig(...), client.WithEndpoints(addrs...))`
   — CACPPT's exact access pattern (CACPPT `controllers/configs.go:68-105`). Endpoint/node choice:
   internal addresses of `UpToDate` control-plane Machines; requests that must land on a specific
   node use `client.WithNode`.
4. Determine the observed Talos version: `client.Version` against the chosen CP node (do not trust
   `spec.controlPlaneConfig.controlplane.talosVersion`, which is absent pre-P1). Desired pair =
   `k8s=<spec.version>,talos=<observed>`.
5. If `last-synced-version` annotation == desired pair and no `force-sync` annotation is present:
   done (also honor `--resync-period` if enabled).
6. Evaluate the gate (G1–G3). If blocked: emit `GateBlocked` event naming the exact blockers
   (machine names, node names, pod versions), update metrics, requeue after
   `--gate-requeue-interval`. If `upgrade.tinkerbell.org/force-sync="true"` is present, skip the
   gate, emit `ForceSyncHonored`, and proceed.
7. Fetch manifests: `manifests.GetBootstrapManifests(client.WithNode(ctx, node), talosClient.COSI, nil)`
   (go-kubernetes@v0.2.38 `kubernetes/manifests/get.go:18` — lists COSI `k8s.Manifest` resources in
   the `controlplane` namespace of the target node).
8. Choose the sync path by observed Talos version, mirroring `PerformManifestsSync`
   (talos@v1.13.5 `talos_managed.go:464-481`): `>= v1.13.0` → SSA with pruning; older → legacy
   `manifests.SyncWithLog` (kept only for completeness; the deployed fleet is v1.13.9).
9. SSA path: `ssa.NewManager(ctx, restCfg, "talos", "kube-system", "talos-bootstrap-manifests-inventory")`
   (go-kubernetes@v0.2.38 `kubernetes/ssa/ssa.go:83`; constant values from machinery@v1.13.0
   `constants/constants.go:1122-1129`). In `--dry-run` mode call `Manager.Diff` and emit the diff as
   events/logs without writing anything (and without updating the annotation). Otherwise call
   `Manager.Apply` with `ApplyOptions{InventoryPolicy: AdoptIfNoInventory, NoPrune: false, Force: --manifests-force, WaitTimeout: --reconcile-timeout}`
   (talosctl's defaults: `cmd/talosctl/cmd/talos/upgrade-k8s.go:63-67` — policy
   `AdoptIfNoInventory`, timeout `5m`), then wait for reconciliation the way `syncManifestsSSA`
   does (`talos_managed.go:556-581`).
10. On success: SSA-apply the `last-synced-version` annotation (field manager
    `talos-upgrade-coordinator`), remove `force-sync` if present, emit `ManifestsSynced` with
    created/configured/pruned counts.
11. On failure: emit `ManifestsSyncFailed`, return the error (backoff requeue). The annotation is
    untouched, so the sync retries.

### Ordering, races, idempotency

- **Mid-rollout**: while CACPPT is rolling/in-place-updating machines, G1 fails — C5 never syncs a
  half-upgraded cluster. This reproduces `upgrade-k8s`'s ordering (manifest sync strictly last).
- **Version changed again mid-flight**: the desired pair is recomputed each reconcile; an
  in-progress sync for a superseded pair is harmless (SSA idempotent) and the next reconcile syncs
  the new pair once its gate passes.
- **Concurrent `talosctl upgrade-k8s`**: same field manager, same inventory ConfigMap — the two
  actors converge instead of conflicting (the Decision above). Worst case is a benign double apply.
- **Partial apply**: `Manager.Apply` prunes only after all applies succeed (go-kubernetes@v0.2.38
  `kubernetes/ssa/apply.go:77-79` doc comment), so a failed run never removes objects prematurely;
  retries are safe.
- **First reconcile ever** (no annotation): C5 evaluates the gate at the *current* pair and syncs
  once.

> **Decision — seed the inventory on first sight.** On a cluster C5 has never synced, the bootstrap
> manifests were applied by machined at bootstrap and belong to no SSA inventory. The first C5 sync
> (policy `AdoptIfNoInventory`) adopts them into `talos-bootstrap-manifests-inventory`, which is a
> prerequisite for pruning to work on the first real upgrade. Running one no-op-shaped sync at the
> current version is therefore intentional, not an accident to be suppressed.

<!-- markdownlint-disable-next-line MD028 -->
> **Decision — checkpoint lives as an annotation on `TalosControlPlane`.** Alternatives considered:
> a C5-owned ConfigMap (invisible next to the object operators actually look at; orphaned on
> cluster delete) and the workload inventory ConfigMap (wrong cluster — unreadable when the
> workload is down, which is exactly when you debug). A metadata-only SSA annotation write with C5's
> own field manager cannot conflict with CACPPT's patch-helper writes and keeps C5's "read-only
> w.r.t. CAPI" posture in every meaningful sense: no spec, no status, no lifecycle influence.

### Limitation (recorded)

Bootstrap manifests derive from the *full* machine config, not just versions: a
`strategicPatches` change that flips a cluster feature (e.g. re-enabling the proxy) alters the
rendered manifests with no version transition, and the pair-keyed trigger misses it. Mitigations:
`--resync-period`, the `force-sync` annotation, or any subsequent version bump. Accepted for v1;
see Open questions.

## Failure modes and degradation

C5 is purely additive: no other component waits on it, it blocks nothing, and its total failure
leaves the cluster exactly as it is today (manifests frozen at bootstrap state). This satisfies the
fail-open philosophy in docs/architecture.md by construction.

| Failure | Behavior |
| --- | --- |
| `<cluster>-kubeconfig` secret absent/malformed | Event `SecretMissing` on the `TalosControlPlane`, metric `gate_blocked{reason="kubeconfig"}`, requeue `5m`. No workload access is attempted. |
| `<cluster>-talosconfig` secret absent/malformed | Event `SecretMissing`, requeue `5m`. |
| Workload API unreachable | Event `WorkloadUnreachable`, exponential backoff. Gate cannot pass (G2/G3 unevaluable) — by design, since syncing blind would be worse. |
| All Talos CP endpoints unreachable | Event `TalosUnreachable`; C5 tries each `UpToDate` CP machine address before giving up for this reconcile; backoff. |
| No `UpToDate` CP machine exists (fresh cluster, full rollout) | Gate blocked; `GateBlocked` names the condition; requeue. |
| Bootstrap node never upgraded (terraform not re-applied) | Gate blocked *forever, visibly*: `GateBlocked` events name the lagging Node and its kubelet version. Operator remedies: upgrade it (correct), `--gate-ignore-nodes` (node object is dead), or `force-sync` (explicit override, one-shot, audit-trailed by the `ForceSyncHonored` event). |
| Partial sync (error mid-apply) | Applied objects stay applied, nothing pruned, annotation not written; retry converges. |
| Mgmt-cluster restart mid-sync | Same as partial sync — the annotation is the only state, written last. |
| CACPPT down | Irrelevant to C5's reads (conditions go stale → gate simply stays blocked); no interaction otherwise. |
| C5 down | No syncs happen; nothing else degrades. Deploy-later is fully supported (first-sight seeding above). |

## RBAC

Management cluster — this component requires a `ClusterRole` (the repo's precedent is
namespace-scoped Roles — F9); all reads, one metadata-patch:

```yaml
kind: ClusterRole
rules:
  - apiGroups: ["controlplane.cluster.x-k8s.io"]
    resources: ["taloscontrolplanes"]
    verbs: ["get", "list", "watch", "patch"]   # patch: annotations only
  - apiGroups: ["cluster.x-k8s.io"]
    resources: ["clusters", "machines"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
```

Plus a namespace `Role` for leader-election `coordination.k8s.io/leases` (`get`, `create`,
`update`) in the release namespace. The cluster-wide `secrets` read is the sore point: RBAC cannot
name-pattern-match `<cluster>-kubeconfig`. The chart's `watchNamespace` value both restricts the
cache and, when set, switches the secrets/CAPI rules to a namespaced `Role`+`RoleBinding` in that
namespace (all CAPI cluster objects live in the `tinkerbell` namespace in this environment) —
recommended deployment shape.

Workload cluster: no RBAC objects shipped; C5 acts with the CAPI kubeconfig's identity
(cluster-admin by CAPI convention), which manifest sync genuinely needs (it applies ClusterRoles,
DaemonSets, CRD-adjacent objects). Talos API: the `os:admin` client config from
`<cluster>-talosconfig`; only COSI reads are performed.

## Deployment

Repo conventions per docs/architecture.md and research on the existing chart:

- Chart `helm/talos-upgrade-coordinator`, binary `cmd/talos-upgrade-coordinator`, image
  `ghcr.io/tinkerbell-community/talos-upgrade-coordinator` (goreleaser: one new `builds[]` id and
  `dockers_v2[]` entry; scratch image, static build, user 65532).
- Deployment: 1 replica, default strategy (no hostNetwork, so no `Recreate` requirement), leader
  election on, ID `talos-upgrade-coordinator.upgrade.tinkerbell.org`. Metrics `:8080`, health
  probes `:8081`, slog factory with `logger=upgrade` component attribute.
- No Service, no TLS, no CRDs, no cert-manager — C5 serves nothing.
- Flags ↔ camelCase values, one flag per value (repo pattern): `--watch-namespace`,
  `--gate-requeue-interval` (`1m`), `--gate-ignore-nodes` (CSV), `--reconcile-timeout` (`5m`),
  `--resync-period` (`0`), `--dry-run` (`false`), `--manifests-force` (`false`),
  `--manifests-no-prune` (`false`), `--leader-elect`, `--metrics-bind-address`,
  `--health-probe-bind-address`, `--log-level`, `--log-format`.

## Dependency strategy

> **Decision — import `siderolabs/go-kubernetes` + the machinery module and port the thin wrapper;
> do not import the full `siderolabs/talos` module; do not import the CACPPT module.** Verified
> against the actual module graphs, 2026-09-01:
>
> - `PerformManifestsSync`, `DetectLowestVersion`, and the `UpgradeProvider` interface live in the
>   **full talos module** (talos@v1.13.5 `pkg/cluster/kubernetes/talos_managed.go:464`,
>   `detect.go:20`, `talos_managed.go:44-48`) — but they are thin glue. All heavy lifting is in the
>   standalone module `github.com/siderolabs/go-kubernetes` (pinned `v0.2.38` by talos v1.13.5):
>   `kubernetes/manifests.GetBootstrapManifests(ctx, state.State, filter)` (`get.go:18`) takes a
>   bare COSI `state.State` — satisfied by the machinery client's `.COSI` field
>   (machinery@v1.13.0 `client/client.go:51,180`); `kubernetes/ssa.NewManager/.Diff/.Apply`
>   (`ssa.go:83`, `diff.go:64`, `apply.go:86`) carry the whole SSA + fluxcd-inventory + pruning
>   engine; `kubernetes/upgrade.NewPath` handles version-path math. The three inventory constants
>   come from machinery (`constants/constants.go:1122-1129`). `DetectLowestVersion` is ~60 lines of
>   client-go + semver with no talos-module dependency at all (`detect.go`).
> - The port is therefore ~150 lines (`internal/upgrade/sync.go` + `detect.go`): our own provider
>   struct replaces `UpgradeProvider`, `getManifests`/`syncManifestsSSA`/`DetectLowestVersion` are
>   re-derived from MPL-2.0 sources — ported files keep the MPL-2.0 header and a provenance
>   comment (file-scoped copyleft; compatible with shipping alongside the repo's license).
> - Why not the full talos module: its go.mod is 426 lines, requires AWS/Azure SDKs and the
>   containerd v2 stack, and — decisive — **replaces `containerd/containerd/v2` with a fork**
>   (talos@v1.13.5 `go.mod:23-25`). Replace directives do not propagate to consumers, so importing
>   `pkg/cluster/kubernetes` builds against upstream containerd instead of the fork Talos was
>   written against: a correctness hazard on top of the weight.
> - Version pins (fork-matching per F7): `github.com/siderolabs/go-kubernetes v0.2.38` (requires
>   machinery `v1.13.0-beta.0`, `k8s.io/* v0.36.0`, controller-runtime `v0.24.0`, `fluxcd/pkg/ssa
>   v0.73.0` — all satisfied by or below the target repo's existing `k8s.io/* v0.37.0` +
>   controller-runtime `v0.24.1`); `github.com/siderolabs/talos/pkg/machinery v1.13.0` (the exact
>   pin of both CABPT and CACPPT go.mod; MVS resolves go-kubernetes's beta floor up to it; a
>   v1.13.0 client against v1.13.9 nodes is same-minor safe); `sigs.k8s.io/cluster-api v1.13.0`
>   (CACPPT's pin) for `Machine` v1beta2 types and `MachineUpToDateCondition`.
> - `TalosControlPlane` is accessed as `unstructured` (3 scalar fields + annotations). Importing
>   the CACPPT API package would require a `replace` of
>   `github.com/siderolabs/cluster-api-control-plane-provider-talos` onto the sidero-community
>   fork — a coupling and go-install hazard not worth three fields.
> - Residual risk, recorded: cluster-api v1.13.0 was built against `k8s.io/* v0.35.3`; compiling
>   its api packages against v0.37.0 is expected to work (API-only import) but is unverified —
>   fallback is unstructured `Machine` access too (one condition + two fields).

## Implementation plan

Packages (single module, repo layout per F9):

- `cmd/talos-upgrade-coordinator/main.go` — flags, slog factory, scheme (client-go + cluster-api
  v1beta2; TCP handled unstructured), manager with optional namespace-scoped cache, reconciler
  wiring, healthz/readyz.
- `internal/upgrade/` (shortname `upgrade`):
  - `reconciler.go` — `type Reconciler struct { client.Client; Recorder record.EventRecorder; Workload WorkloadFactory; Talos TalosFactory; Opts Options }`; `func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)` implementing the flow above; `func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error` — `For(tcpUnstructured, builder.WithPredicates(generationOrUpgradeAnnotationChanged))`, `Watches(&clusterv1.Machine{}, handler.EnqueueRequestsFromMapFunc(r.machineToTCP), builder.WithPredicates(upToDateOrVersionChanged))`.
  - `tcp.go` — unstructured accessors: `func TCPVersion(u *unstructured.Unstructured) (string, error)`, `func TCPTalosVersion(u *unstructured.Unstructured) string`, `func LastSynced(u *unstructured.Unstructured) string`, `func ClusterNameFor(ctx, c client.Client, u *unstructured.Unstructured) (string, error)`.
  - `gate.go` — `type GateResult struct { Converged bool; Blockers []string }`;
    `func EvaluateGate(ctx context.Context, mgmt client.Client, workload kubernetes.Interface, cluster string, ns string, target semver.Version, ignoreNodes sets.Set[string]) (GateResult, error)` (G1 Machines, G2 Nodes, G3 static-pod scan ported from `detect.go`).
  - `sync.go` (MPL-2.0 header) — `type SyncOptions struct { DryRun, Force, NoPrune bool; ReconcileTimeout time.Duration; Log func(string, ...any) }`; `type SyncReport struct { Created, Configured, Unchanged, Pruned int }`;
    `func SyncBootstrapManifests(ctx context.Context, cosi state.State, restCfg *rest.Config, talosVersion semver.Version, o SyncOptions) (SyncReport, error)` — the `PerformManifestsSync` port.
  - `talosclient.go` — `func NewTalosClient(ctx context.Context, talosconfig []byte, endpoints []string) (*client.Client, error)`; `func ObservedTalosVersion(ctx context.Context, c *client.Client, node string) (semver.Version, error)`.
  - `workload.go` — `func WorkloadRESTConfig(ctx context.Context, c client.Client, ns, cluster string) (*rest.Config, error)` (secret `<cluster>-kubeconfig` key `value`).
  - `annotations.go` — SSA metadata-apply of `last-synced-version`, force-sync consume/remove;
    keys as constants.

Factories (`WorkloadFactory`, `TalosFactory`) are interfaces so tests can inject fakes.

Milestones:

- **M1**: binary + chart + RBAC; TCP/Machine watches; pair computation and annotation bookkeeping;
  sync stubbed to `Diff`-only (`--dry-run` hardwired). Deployable and observable, zero write risk.
- **M2**: full gate (G1–G3) with events, metrics (`upgrade_gate_blocked{reason}`,
  `upgrade_last_sync_timestamp_seconds`, `upgrade_sync_total{result}`,
  `upgrade_manifests_pruned_total`), `--gate-ignore-nodes`.
- **M3**: real SSA apply + prune + wait, legacy non-SSA path, `force-sync`, `--resync-period`.
- **M4**: hardening — homelab e2e (v1.36 minor bump on the live cluster), fold-in evaluation notes
  for CACPPT.

Test strategy:

- Unit (table-driven, repo convention): gate logic against `k8s.io/client-go/kubernetes/fake`
  clientsets (node/pod fixtures incl. a lagging bootstrap node) and controller-runtime fake client
  Machines with/without the `UpToDate` condition; `tcp.go` accessors; annotation state machine
  (pair transitions, force-sync consumption, failed-sync retry).
- Sync integration: seed an in-memory COSI state (`cosi-project/runtime` `state.WrapCore` +
  `inmem`) with `k8s.Manifest` resources; target an envtest API server as the "workload" cluster;
  assert SSA apply results, inventory ConfigMap contents, prune-on-removal, dry-run writes nothing.
  No fake Talos gRPC server is needed for the manifest path — the COSI `state.State` seam is the
  injection point; `ObservedTalosVersion` is faked at the `TalosFactory` interface.
- Reconciler integration: envtest as the management cluster with the CACPPT `TalosControlPlane` CRD
  and cluster-api Machine CRDs installed from their fork manifests; stub factories; drive version
  transitions and assert gate/requeue/annotation behavior end to end.

## Non-goals

- Machine-level anything: no `ApplyConfiguration`, no `Upgrade`, no per-node config patches —
  kube-proxy included (F3; scope rationale above). C5 never marks hooks pending, never annotates
  Machines, never influences rollouts.
- Ordered control-plane rollout, kubelet updates, image pre-pull — the in-place pipeline
  (CACPPT + CABPT) owns per-node progression.
- Adopting the terraform bootstrap node into CAPI (architecture non-goal); C5 compensates for its
  invisibility at the gate instead.
- Writing `TalosControlPlane.status` or conditions (CACPPT's), or any Tinkerbell object.
- Talos version upgrades of any node (P3's domain, via CAPT/CABPT).

**Fold-into-CACPPT (future note)**: C5 shadows the control-plane provider's domain and is the one
component whose long-term home is plausibly CACPPT itself (a post-rollout manifest-sync phase after
`reconcileMachines`). It stays separate now because (a) its failure domain is the workload cluster's
full API surface — a place the CP provider deliberately touches narrowly, (b) independent
deployability lets it ship and iterate without fork-release coupling, and (c) the annotation +
field-manager contracts defined here are designed to survive a future fold-in unchanged (CACPPT
would simply become the writer of `upgrade.tinkerbell.org/last-synced-version`).

## Open questions

- Does cluster-api v1.13.0's `api/core/v1beta2` package compile cleanly against
  `k8s.io/* v0.37.0`? Expected yes (API-only), verified only at the go.mod level; fallback is
  unstructured Machine access.
- Should the Talos-side COSI read use a least-privilege `os:reader` talosconfig instead of the
  `os:admin` secret? CABPT only publishes the admin config today; a reader-scoped derived secret
  would be a CABPT cross-repo nicety, not a C5 blocker.
- Config-driven manifest changes without a version transition (the recorded limitation): is an
  opt-in watch on the `<cluster>-talos`/bootstrap-data hash worth the added coupling, or does
  `--resync-period` suffice in practice?
- When Talos v1.14 lands, does `go-kubernetes`'s compatibility surface (`kubernetes/compatibility`)
  need to gate the pair computation (supported upgrade paths via `upgrade.NewPath.IsSupported`)
  before syncing, or is the gate's minor-equality check sufficient?
