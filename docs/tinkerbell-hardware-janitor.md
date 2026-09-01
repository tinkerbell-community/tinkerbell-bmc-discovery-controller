# tinkerbell-hardware-janitor (C4)

Status: Proposed — 2026-09-01

The janitor is a small controller that restores released Tinkerbell `Hardware` to a clean, safe
baseline after CAPT gives it up. CAPT's `releaseHardware` removes only owner labels, the provisioned
annotation, and finalizers — it never clears `Hardware.spec.userData`, which carries the full
Talos machineconfig (cluster CA keys, bootstrap token, etcd secrets) and which tootles keeps serving
unauthenticated at `/2009-04-04/user-data` for whoever holds that machine's IP. The janitor watches
for the released state, and in a single optimistic-concurrency-guarded update clears `spec.userData`,
clears `spec.metadata.instance.operating_system` and `spec.metadata.instance.state`, and parks the
netboot posture (`allowPXE=false`). It acts on every release path — including force-deletes where
C3 (`talos-machine-teardown`) never ran — and, by construction of its predicate, can never touch
claimed hardware, never-claimed inventory, or the terraform-managed bootstrap node.

## Purpose and scope

- Scrub secret-bearing and stale state from `tinkerbell.org` `Hardware` objects that CAPT has
  released, so freed hardware stops serving the old cluster's machine config and metadata.
- Re-assert a parked netboot posture on released hardware only, closing the stranded
  `allowPXE=true` gap left when a running Workflow is deleted during machine teardown.
- Cover every release path uniformly: normal delete (after C3 and CAPT power-off), MHC
  remediation, out-of-band `kubectl delete`, whole-cluster delete, and paths where C3 never ran.

Out of scope: everything on claimed hardware, BMC or power actions, disk wipe (C3's Talos reset
plus the next provision's `install.wipe` cover it), C1's classification labels, and deleting any
Kubernetes object. Cross-component contracts, the ownership matrix, and preconditions P1–P7 live in
`docs/architecture.md`; this doc covers only C4.

## Context

Three governing facts from `docs/architecture.md` shape this component:

- F4: CAPT teardown is Template CR + Workflow CR deletion, one rufio `PowerHardOff` job, then
  hardware release — `Hardware.spec.userData` is never cleared
  (cluster-api-provider-tinkerbell `controller/machine/scope.go:292-368`,
  `controller/machine/hardware.go:347-365`).
- F5: `Hardware.spec.metadata.instance.operating_system` feeds tootles EC2 metadata and the
  deployed workflow template's `IMG_URL`; while hardware is claimed, C2 (`talos-os-metadata`) is
  its writer. On release, clearing is handed off to C4 (state-disjoint handoff).
- C0: the discovery controller's SSA migration (see `docs/discovery-field-ownership.md`) is what
  makes long-lived coexistence on `Hardware` safe. Until C0 lands, discovery's hourly full-spec
  rewrite can undo the janitor's parked posture (details under Failure modes).

Preconditions that touch C4: P4 (terraform must `ignore_changes` on
`metadata.instance.operating_system` and netboot fields for terraform-created `Hardware`, or a
subsequent `terraform apply` re-asserts what C4 cleared) and P6 (all `Hardware` lives in the
`tinkerbell` namespace, which lets C4 stay namespace-scoped).

### The secrets-hygiene rationale, with evidence

`releaseHardware` in the CAPT fork does exactly this and nothing more
(cluster-api-provider-tinkerbell `controller/machine/hardware.go:347-365`):

```go
delete(hw.Labels, HardwareOwnerNameLabel)      // v1alpha1.tinkerbell.org/ownerName
delete(hw.Labels, HardwareOwnerNamespaceLabel) // v1alpha1.tinkerbell.org/ownerNamespace
delete(hw.Annotations, HardwareProvisionedAnnotation) // v1alpha1.tinkerbell.org/provisioned
controllerutil.RemoveFinalizer(hw, infrastructurev1.MachineFinalizer)
controllerutil.RemoveFinalizer(hw, infrastructurev1.MachineLegacyFinalizer)
```

`UserData` is not touched. Meanwhile tootles serves it verbatim, keyed only by requesting IP
(tinkerbell `tootles/internal/backend/backend.go:144-146`):

```go
if hw.Spec.UserData != nil {
    i.Userdata = *hw.Spec.UserData
}
```

The value is the complete CABPT-rendered Talos machineconfig with `PROVIDER_ID` substituted
(cluster-api-provider-tinkerbell `controller/machine/hardware.go:169-185`) — cluster CA key,
bootstrap token, etcd CA and friends. Released hardware keeps a static DHCP reservation in
`spec.interfaces[].dhcp` (terraform-written), so any device that acquires or spoofs that IP on the
provisioning network can fetch full cluster-admin material with a single unauthenticated GET until
a future claim happens to overwrite it. Clearing on release bounds that exposure window to the
janitor's reaction time. (Scrubbing prevents future serving; it does not revoke secrets already
leaked — rotation is out of scope and noted in Non-goals.)

## Contracts

### Reads

| Object | Field | Owner (writer) |
| --- | --- | --- |
| `Hardware.metadata.labels` | `v1alpha1.tinkerbell.org/ownerName`, `.../ownerNamespace` | CAPT (claim/release) |
| `Hardware.metadata.annotations` | `v1alpha1.tinkerbell.org/provisioned` | CAPT (workflow success/release); terraform pre-sets it on the bootstrap node |
| `Hardware.spec.userData` | presence/absence | CAPT (sole writer) |
| `Hardware.spec.metadata.instance.operating_system` | presence (for change detection only) | C2 while claimed; terraform pre-P4 |
| `Hardware.spec.interfaces[].netboot.allowPXE` | current value | tink workflow controller during workflow lifecycle; terraform at create |

### Writes

All writes go through a single resourceVersion-guarded `Update` (never a merge patch, never SSA
Apply — see Design), with field owner `tinkerbell-hardware-janitor` recorded in `managedFields`.

| Object | Field | Write |
| --- | --- | --- |
| `Hardware.spec.userData` | set to `nil` | released state only |
| `Hardware.spec.metadata.instance.operating_system` | set to `nil` | released state only; gated by `--clear-os-metadata` |
| `Hardware.spec.metadata.instance.state` | set to `""` | released state only |
| `Hardware.spec.interfaces[].netboot.allowPXE` | set to `false` (baseline) on interfaces that already carry a `netboot` block | released state only; gated by `--baseline-allow-pxe` |
| `Hardware.metadata.annotations` | `janitor.tinkerbell.org/scrubbed-at=<RFC3339>` | audit stamp, same update |

Owned keys (per the naming standard in `docs/architecture.md`):

- Annotation `janitor.tinkerbell.org/scrubbed-at` — C4 exclusive; informational; survives
  re-claims (records the last scrub).
- Event reasons on `Hardware`: `HardwareScrubbed`, `HardwareHalfReleased`.

C4 owns no labels and no conditions. It must never write: `bmcRef`, `agentID`,
`interfaces[].dhcp`, `disks`, `metadata.manufacturer`, `metadata.facility` (discovery-owned, C0),
`classify.tinkerbell.org/*` and `talos.tinkerbell.org/*` keys (C1), or anything on claimed
hardware.

## Design

### The released-hardware predicate

Let three booleans describe a `Hardware` object:

- `O` — owner labels absent (`v1alpha1.tinkerbell.org/ownerName` and `.../ownerNamespace` both
  unset).
- `U` — `spec.userData` non-empty (non-nil and non-blank after trimming whitespace).
- `P` — provisioned annotation absent (`v1alpha1.tinkerbell.org/provisioned` unset).

C4 acts if and only if `O AND U AND P`.

| `O` (no owner) | `U` (userData set) | `P` (no provisioned) | Classification | Janitor action |
| --- | --- | --- | --- | --- |
| false | any | any | Claimed (4 rows) | none — CAPT's hardware |
| true | false | true | Never claimed | none — nothing to scrub |
| true | false | false | Bootstrap-reserved (terraform node) | none — must never touch |
| true | true | false | Half-released (anomaly) | no write; emit `HardwareHalfReleased` event + metric |
| true | true | true | Released | scrub |

Why each conjunct exists:

- **Owner labels absent (`O`).** Claimed hardware belongs to a live or in-flight machine: its
  `userData` is the config tootles must keep serving (the node re-fetches at boot via the baked
  `talos.config` kernel arg), and its OS metadata is the invariant C2's webhook just admitted a
  Workflow against. Touching any of it while owned would brick installs or violate C2's guarantee.
  CAPT sets these labels at claim and removes them at release
  (`controller/machine/hardware.go:22-38,347-365`), so their absence is the authoritative
  "CAPT is done with this hardware" signal.
- **`spec.userData` non-empty (`U`).** Only CAPT writes `spec.userData` in this environment: the
  discovery controller's `DesiredHardwareSpec` never sets it
  (tinkerbell-bmc-discovery-controller `internal/sync/mapping.go:69-111`), terraform marks it as a
  computed field and its `user-data` module is dead code
  (tfc/cluster-bootstrap `modules/node/modules/hardware/locals.tf:7-22`). Non-empty `userData`
  therefore means "this hardware was actually claimed and configured at some point" — which is the
  only hardware with anything to scrub. This conjunct excludes never-claimed inventory (fresh
  discovery output, auto-enrolled hardware) so the janitor produces zero writes on the fleet's
  growth path, and it makes the action one-shot: clearing `userData` falsifies `U`, so a scrubbed
  object permanently leaves the predicate set (self-extinguishing — see Idempotency).
- **Provisioned annotation absent (`P`).** `releaseHardware` deletes
  `v1alpha1.tinkerbell.org/provisioned` (`hardware.go:355`), so genuine releases always satisfy
  this. Its presence with no owner labels means one of two things, and both are no-touch: the
  terraform bootstrap node (below), or a half-completed manual release — where scrubbing on
  guesswork is worse than surfacing an event.

**The terraform bootstrap node, walked explicitly.** `machines[0]` is provisioned entirely outside
Tinkerbell (Redfish virtual media + `talos_machine_configuration_apply`), is excluded from CAPI
(`TalosControlPlane.spec.replicas = n-1`), and its `Hardware` is created by terraform with
`allowPXE=false` and the annotation `v1alpha1.tinkerbell.org/provisioned="true"` pre-set precisely
so CAPT never claims or re-images it (tfc/cluster-bootstrap `modules/cluster/main.tf:177-178`;
CAPT `controller/machine/scope.go:196-214`). Its predicate evaluation: `O=true` (never claimed),
`U=false` (terraform never writes `spec.userData`; its config was applied over the network),
`P=false` (annotation present). It fails **two independent conjuncts**, so a bug or race in either
one still leaves it protected. This matters most for the netboot action: flipping this node's
`allowPXE` to any value, or clearing its metadata, would tamper with the one machine that anchors
the whole management cluster. A predicate of "owner labels absent" alone — the naive reading of
"released" — would have matched it; that is exactly the critique finding this predicate encodes.

**Half-released anomaly (`O AND U AND NOT P`).** Reachable only by a manual partial release
(operator removed labels but not the annotation) or a hypothetical future writer of `userData` on
unclaimed hardware. The janitor emits a `HardwareHalfReleased` event and increments
`janitor_half_released_hardware` but does not write: the state is ambiguous, and the fix is a
documented runbook step (remove the annotation too, after which the normal path runs). Note the
bootstrap node can never land here because `U=false` for it.

### Actions and their safety arguments

All three mutations happen in one update, so observers see either the pre-release state or the
fully scrubbed state, never a partial scrub.

1. **Clear `spec.userData`.** Safety: the predicate guarantees no owner, so no running or
   in-flight machine depends on tootles serving this config (a machine's config is fetched at boot
   via the image-baked `talos.config=...user-data` URL; a released machine must not boot into the
   old cluster again anyway). The one nuance is BMC-less hardware: CAPT releases it *without* a
   power-off (`scope.go:292-368`, the `BMCRef==nil` branch), so the old node may still be running
   when C4 scrubs. That is still safe: the running node holds its config in memory and on its
   `STATE` partition; the effect of the clear is only that a reboot fails to re-fetch config — the
   desired outcome for an evicted machine.
2. **Clear `metadata.instance.operating_system` and `metadata.instance.state`.** Safety: no
   consumer reads these for unowned hardware at runtime; tootles' `operating-system` endpoints and
   the deployed template's `IMG_URL` read them only for a machine that is booting or claimed
   (tinkerbell `api/v1alpha1/tinkerbell/hardware.go:319-341`; tfc/cluster-bootstrap
   `modules/cluster/templates/template-data.yaml:20-52`). The positive argument: C2's webhook
   distinguishes "absent" from "stale" trivially when release always clears — a value left over
   from the previous claim can look superficially valid, but absence is unambiguous, so the
   C4-clear plus C2-mirror pair enforces fresh metadata per claim. This is the state-disjoint
   handoff recorded in the architecture ownership matrix: C2 writes while claimed, C4 clears at
   release, and the two states cannot overlap.
3. **Re-assert baseline netboot posture: `allowPXE=false` on every interface that has a `netboot`
   block.** Safety: restricted to released state, so it can never fight the tink workflow
   controller, which owns `allowPXE` during a workflow's lifecycle (sets `true` in `PREPARING`,
   `false` in `POST` — tinkerbell `tink/controller/internal/workflow/hardware.go:36-166`,
   `pre.go:22-28`, `post.go:16-23`) — workflows for CAPT-claimed hardware imply owner labels, which
   put the object outside C4's predicate. The action exists for the stranded case: Workflow
   deletion during teardown short-circuits the reconciler (no finalizers, tinkerbell
   `tink/controller/internal/workflow/reconciler.go:137-139`), so a machine deleted mid-provision
   leaves `allowPXE=true` on released hardware. Parked-false prevents an unowned machine with a
   stale OS on disk from netbooting into HookOS (and, with this environment's auto-enrollment
   enabled, from triggering an enrollment Workflow) on a manual power-on. `false` is also the
   posture the stack itself converges to (tink `POST`) and what terraform sets for the parked
   bootstrap node. Re-arming for the next claim is not C4's job: CAPT always creates Workflows with
   `ToggleAllowNetboot=true` and the tink controller flips `allowPXE` back on
   (CAPT `controller/machine/workflow.go:40-97`).

> **Decision — baseline value is `allowPXE=false`.** The charter mandates "baseline netboot
> posture" without fixing the value. `false` is chosen because it matches the stack's own
> end-of-workflow posture, the bootstrap node's posture, and the safety argument above; `true`
> would only help if the next claim's `ToggleAllowNetboot` were unreliable, which it is not. The
> value is exposed as `--baseline-allow-pxe` (default `false`) so an environment that wants
> always-armed PXE can opt in explicitly rather than by fork.

A second flag-level decision concerns the OS-metadata clear:

> **Decision — `--clear-os-metadata` defaults to `true`, with an escape hatch.** Under the current
> deployed template, `IMG_URL` is composed from Hardware OS metadata at render time; if C4 clears
> it and neither C2 nor terraform repopulates before the next claim (P1 unmet, C2 undeployed), the
> next Workflow render fails (`missingkey=error`). The default follows the charter (clearing is
> C4's half of the C2 handoff); the flag lets an environment still on terraform-written OS
> metadata deploy C4 for the userData scrub alone without breaking re-provisioning.

### Concurrency safety: resourceVersion-guarded update

The write primitive is a plain `Update` of a mutated copy of the observed object, carrying the
observed `resourceVersion` (client-go sends it automatically on `Update`). Never a merge patch,
and never SSA Apply.

The interleaving this defends against:

1. `t0` — CAPT `releaseHardware` lands; Hardware at `resourceVersion=100`, labels/annotation gone,
   `userData` still set.
2. `t1` — C4's watch fires; reconcile reads the object at `rv=100`; predicate matches; it builds
   the scrubbed object.
3. `t2` — a **new** TinkerbellMachine claims the same hardware: CAPT's optimistic-lock patch adds
   owner labels (`rv=101`), then writes the new machine's `userData` (`rv=102`;
   `controller/machine/hardware.go:169-185`). C2 re-mirrors OS metadata; its webhook admits the
   new Workflow against it.
4. `t3` — C4's write from step 2 arrives late (slow janitor, stale cache, requeue backlog).

With a blind merge patch (`{"spec":{"userData":null,...}}`), step 4 succeeds regardless of the
intervening claim: it erases the *new* machine's config, so the node netboots, fetches empty
user-data, and boots configless into maintenance mode — a bricked install that CAPT will never
repair, because the provisioned short-circuit means `userData` is written once per claim. It also
nulls the OS metadata that C2's webhook just vouched for, silently violating that invariant after
admission. With the guarded update, step 4 is rejected `409 Conflict` because the object is at
`rv=102`, not `100`; the reconciler requeues, re-reads, sees owner labels present, and no-ops.

SSA Apply is the wrong primitive here even though it is the repo standard: SSA conflicts are
field-manager-scoped, not state-scoped. C4 does not own `userData` (CAPT's manager does), so
clearing it via SSA would require a force-apply that takes ownership — and a force-apply carries no
notion of "the state I observed", so it lands at `t3` just like the blind patch. C4's write is
inherently "delete these fields *given the released state I saw*", which is exactly optimistic
concurrency.

> **Decision — C4 deviates from the repo-wide SSA standard, deliberately and narrowly.** Writes use
> `client.Update` with `client.FieldOwner("tinkerbell-hardware-janitor")` (so `managedFields`
> still attributes the write for audit), and on `IsConflict` the reconciler returns the error to
> the workqueue — a fresh reconcile re-reads and re-classifies from scratch. In-process
> `retry.RetryOnConflict` loops that re-apply the same mutation to a re-read object are forbidden
> in this component: re-classification, not re-application, is the correctness mechanism. This
> exception is recorded in the architecture ownership matrix.

### Watch and predicate wiring

Label-absence is awkward to express in controller-runtime event predicates, but it is directly
expressible as a server-side *label selector*: `!v1alpha1.tinkerbell.org/ownerName` (a
`DoesNotExist` requirement). The kube-apiserver evaluates it on the watch and synthesizes an
`ADDED` event when an object starts matching (release removes the label) and a `DELETED` event
when it stops (re-claim adds it).

> **Decision — filtered cache, not watch-all-plus-index.** C4 configures its manager cache with
> `cache.Options.ByObject[&tinkv1.Hardware{}].Label = labels.Parse("!v1alpha1.tinkerbell.org/ownerName")`,
> scoped to the target namespace via `DefaultNamespaces`. Consequences: (a) the release transition
> arrives as a plain Add event — no custom old/new label-diff predicate needed; (b) the janitor's
> informer never caches claimed hardware at all, so it holds no live machineconfigs in memory
> beyond the released window; (c) API server and controller load are minimal. The remaining two
> conjuncts (`U`, `P`) are not expressible server-side and are evaluated in `Reconcile`. The
> alternative in the charter's parenthetical — watch all Hardware and filter in reconcile with an
> index — was rejected: it caches every claimed object's secret-bearing spec for no benefit, and
> the index adds nothing the selector does not already provide. Correctness does not depend on the
> selector (a stale or missing event is caught by the informer resync, and the write guard holds
> regardless); the selector is load- and hygiene-motivated.

Reconcile flow, step by step:

1. `Get` the `Hardware` from the (filtered) cache. `NotFound` means deleted or re-claimed out of
   the selector — return, done.
2. `Classify(hw)` per the truth table.
   - `Claimed` (only possible transiently, via a just-stale cache): return.
   - `NeverClaimed` / `BootstrapReserved`: return.
   - `HalfReleased`: emit `HardwareHalfReleased` event (recorder deduplicates), bump the gauge,
     return without requeue — the informer resync revisits it.
   - `Released`: continue.
3. `scrubbed := hw.DeepCopy()`; apply `Scrub(scrubbed, opts)` — mutate only the target fields
   (never rebuild the spec); if nothing changed, return (idempotent re-entry).
4. `Update(ctx, scrubbed, client.FieldOwner(FieldManager))`.
   - `IsConflict`: return the error (workqueue backoff → fresh read → re-classify).
   - other error: return it.
5. Emit `HardwareScrubbed` event naming the fields cleared; bump
   `janitor_hardware_scrubbed_total`.

Idempotency: the scrub falsifies `U`, so a scrubbed object classifies as `NeverClaimed` forever
after — the predicate is self-extinguishing and C4 writes at most once per release transition.
Replayed events and resyncs hit step 3's `changed == false` short-circuit. The
`janitor.tinkerbell.org/scrubbed-at` annotation is refreshed only when an actual scrub happens.

### Interaction table

| Peer | Path | Interaction and ordering |
| --- | --- | --- |
| C3 `talos-machine-teardown` | normal delete | C3 completes etcd-leave/reset at pre-terminate, CAPI removes the hook, CAPT deletes Template+Workflow, powers off, then `releaseHardware` — which is what makes C4's predicate true. Because release is CAPT's last step (`scope.go:292-368`), C4 always runs after power-off on this path and never scrubs under a live node. |
| C3 skipped | force-delete, C3 down, pre-C3 machines | C4 alone provides the hardware-hygiene half; the dirty disk residual is covered by the next provision's `install.wipe`. If CAPT's release itself never ran (finalizers force-removed by hand), the predicate cannot match — the runbook step is to mirror `releaseHardware` manually (remove both owner labels and the provisioned annotation), after which C4 does the rest. |
| C2 `talos-os-metadata` | handoff | State-disjoint: C2 writes `operating_system` only for claimed hardware; C4 clears it only when released. No shared state, no write fight. The clear is what makes C2's staleness check trivial (absent vs. present, never stale-but-plausible). C4's `Update` removes C2's `managedFields` entry for the cleared fields; C2 re-applies fresh on the next claim. |
| C0 discovery controller | coexistence | Post-C0, discovery's SSA apply asserts only its own fields (`bmcRef`, `agentID`, interface MAC/hostname/dhcp, disks, manufacturer/facility) — none of which C4 writes, and discovery asserts none of what C4 clears. The janitor must never clear discovery-owned fields; its mutate-in-place scrub guarantees that structurally. Pre-C0 hazards: see Failure modes. |
| tink workflow controller | `allowPXE` ownership | Owns `allowPXE` during a workflow's lifecycle (`PREPARING` true, `POST` false). C4 asserts the parked posture exactly once, in released state, where no CAPT workflow can exist (CAPT deletes the Workflow before releasing). Residual overlap: an auto-enrollment Workflow could theoretically target hardware in C4's released window; worst case is one 409 on either side, resolved by retry, after which C4 is extinct for that object. |
| CAPT | producer | CAPT is both the claimer (writes `userData`, `hardware.go:169-185`) and the releaser (`hardware.go:347-365`); C4 is purely reactive to the release and touches nothing CAPT still owns. |
| terraform | bootstrap node, P4 | The bootstrap node is protected by two conjuncts (walked above). For terraform-created but CAPI-managed Hardware, C4's OS-metadata clear is durable only once P4 (`ignore_changes`) lands; before that, a `terraform apply` re-asserts the OS object — visible drift, not a correctness hazard. |

## Failure modes and degradation

Consistent with the fail-open philosophy in `docs/architecture.md`: the janitor degrades to "scrub
later", never blocks any other component, and surfaces anomalies as events/metrics.

- **Janitor down or undeployed.** Released hardware keeps serving the old machineconfig until the
  janitor returns — exactly today's (pre-C4) behavior, bounded by the outage. On restart the
  filtered informer lists all currently-released hardware and reconciles it; level-triggered, so
  no release is ever permanently missed.
- **Stale cache / watch lag.** Correctness is carried entirely by the resourceVersion guard; lag
  only delays scrubs or produces harmless 409s.
- **C2 absent or P1 unmet.** With `--clear-os-metadata=true`, released-then-reclaimed hardware
  cannot render the current template (`IMG_URL` inputs missing) until some writer repopulates OS
  metadata — CAPT retries workflow creation with backoff, so this is a visible stall, not silent
  corruption; the flag is the documented mitigation for pre-C2 environments.
- **C0 not yet landed.** The discovery controller's hourly full-spec rewrite
  (tinkerbell-bmc-discovery-controller `internal/sync/syncer.go:84-124`) interacts two ways with
  C4 on discovery-managed hardware: it re-asserts `allowPXE=true`, silently undoing the parked
  posture (C4 will not re-act — its predicate is already extinct for scrubbed objects); and it can
  itself erase `userData`/OS metadata before C4 gets there, in which case C4 classifies the object
  `NeverClaimed` and skips. Terraform-created (discovery-unmanaged) hardware is unaffected. Net:
  C4 is still correct pre-C0, but its parked posture is only durable post-C0.
- **Half-released hardware.** Never auto-repaired; surfaced via `HardwareHalfReleased` events and
  the `janitor_half_released_hardware` gauge until an operator completes the release.
- **API server unavailable.** Standard controller-runtime backoff; no state is held outside the
  cluster, so recovery is a plain re-list.

## RBAC

Namespace-scoped `Role` + `RoleBinding` in the target namespace (default `tinkerbell`, per P6),
matching the repo's namespace-scoped precedent — C4 needs no cluster-scoped or CAPI access at all:

```yaml
rules:
  - apiGroups: ["tinkerbell.org"]
    resources: ["hardware"]
    verbs: ["get", "list", "watch", "update"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "create", "update"]
```

No `patch` on `hardware` (the component never patches), no secrets, no `bmc.tinkerbell.org`, no
`cluster.x-k8s.io`.

## Deployment

Chart `helm/tinkerbell-hardware-janitor` following the repo conventions from
`docs/architecture.md` and the existing discovery chart: chart name == binary == image ==
Deployment/SA/Role name; kebab-case flags mapping 1:1 to camelCase values; image tag defaults to
`.Chart.AppVersion` (no `v` prefix); chart version independent of `appVersion`.

- `Deployment`: 1 replica, default `RollingUpdate` (no hostNetwork, unlike discovery), leader
  election on, `runAsNonRoot` 65532, metrics `:8080`, probes `:8081` (`/healthz`, `/readyz`).
- No `Service`, no TLS, no cert-manager, no CRDs — this is the simplest chart in the repo.
- goreleaser: append build id `tinkerbell-hardware-janitor` (main
  `./cmd/tinkerbell-hardware-janitor`) and a matching `dockers_v2` image
  `ghcr.io/tinkerbell-community/tinkerbell-hardware-janitor` to the existing lists.

Flags and values:

| Flag | Value key | Default | Meaning |
| --- | --- | --- | --- |
| `--namespace` | `namespace` | release namespace | namespace watched for `Hardware` |
| `--clear-os-metadata` | `clearOsMetadata` | `true` | clear `operating_system` + `instance.state` on scrub |
| `--baseline-allow-pxe` | `baselineAllowPxe` | `false` | parked `allowPXE` value asserted on scrub |
| `--resync-period` | `resyncPeriod` | `1h` | informer resync; bounds the catch-up window for missed events |
| `--leader-elect` | `leaderElect` | `true` | leader election ID `tinkerbell-hardware-janitor.janitor.tinkerbell.org` |
| `--metrics-bind-address` | `metricsBindAddress` | `:8080` | Prometheus metrics |
| `--health-probe-bind-address` | `healthProbeBindAddress` | `:8081` | probes |
| `--log-level`, `--log-format` | `logLevel`, `logFormat` | `info`, `json` | slog factory (`internal/logging`) |

## Implementation plan

Single Go module (existing), new binary `cmd/tinkerbell-hardware-janitor/main.go`, package
`internal/janitor/`. Dependencies: already-present `tinkerbell/tinkerbell/api` (Hardware types)
and controller-runtime; no CAPT module import — the foreign contract keys are redeclared locally.

```go
// internal/janitor/contract.go — foreign contract keys, with provenance comments.
const (
    OwnerNameLabel        = "v1alpha1.tinkerbell.org/ownerName"      // CAPT hardware.go:27
    OwnerNamespaceLabel   = "v1alpha1.tinkerbell.org/ownerNamespace" // CAPT hardware.go:31
    ProvisionedAnnotation = "v1alpha1.tinkerbell.org/provisioned"    // CAPT hardware.go:34
    ScrubbedAtAnnotation  = "janitor.tinkerbell.org/scrubbed-at"     // C4-owned
    FieldManager          = "tinkerbell-hardware-janitor"
)
```

```go
// internal/janitor/classify.go
type Class int

const (
    ClassClaimed Class = iota
    ClassNeverClaimed
    ClassBootstrapReserved // no owner, no userData, provisioned annotation present
    ClassHalfReleased      // no owner, userData present, provisioned annotation present
    ClassReleased          // no owner, userData present, no provisioned annotation
)

func Classify(hw *tinkv1.Hardware) Class
```

```go
// internal/janitor/scrub.go
type Options struct {
    ClearOSMetadata  bool
    BaselineAllowPXE bool
    Now              func() time.Time
}

// Scrub mutates hw in place (userData, os metadata, instance state, netboot
// posture, scrubbed-at annotation) and reports whether anything changed.
func Scrub(hw *tinkv1.Hardware, opts Options) (changed bool)
```

```go
// internal/janitor/reconciler.go
type Reconciler struct {
    client.Client
    Recorder record.EventRecorder
    Opts     Options
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error // For(&tinkv1.Hardware{})
```

Manager wiring in `main.go` (the filtered cache is the notable part):

```go
sel, _ := labels.Parse("!" + janitor.OwnerNameLabel)
mgr, err := ctrl.NewManager(cfg, ctrl.Options{
    Scheme:           scheme, // clientgoscheme + tinkv1
    LeaderElection:   leaderElect,
    LeaderElectionID: "tinkerbell-hardware-janitor.janitor.tinkerbell.org",
    Cache: cache.Options{
        SyncPeriod:        &resyncPeriod,
        DefaultNamespaces: map[string]cache.Config{namespace: {}},
        ByObject: map[client.Object]cache.ByObject{
            &tinkv1.Hardware{}: {Label: sel},
        },
    },
})
```

Metrics: `janitor_hardware_scrubbed_total` (counter), `janitor_update_conflicts_total` (counter),
`janitor_half_released_hardware` (gauge).

Milestones:

- **M1 — pure logic.** `contract.go`, `Classify`, `Scrub` with table-driven tests covering the
  full truth table (all 8 rows, including a bootstrap-node fixture mirroring the terraform-created
  object: `allowPXE=false`, provisioned annotation, empty `userData`), whitespace-only `userData`,
  idempotent re-scrub (`changed == false`), and netboot mutation only on interfaces that already
  carry a `netboot` block.
- **M2 — reconciler + envtest.** Real API server with the tinkerbell `Hardware` CRD installed.
  Tests: (a) release transition delivers an event through the label-selector watch and the object
  gets scrubbed; (b) the race test — reconcile reads the released object, a second client
  re-claims (adds owner labels, writes new `userData`) before the janitor's update, assert the
  update returns `409` and the follow-up reconcile leaves the new `userData` intact; (c)
  half-released objects produce an event and no write; (d) self-extinguishing — a second reconcile
  of a scrubbed object performs no update (assert `resourceVersion` unchanged).
- **M3 — packaging.** `cmd/` main, chart, goreleaser build+image entries, CI (`helm-lint`,
  existing lint/test jobs pick the package up automatically), events and metrics wired.
- **M4 — integration.** kind cluster with the tinkerbell CRDs: scripted CAPT-shaped claim
  (labels + `userData` + provisioned annotation) and release (label/annotation removal), assert
  scrub, parked posture, and non-action on bootstrap-shaped and never-claimed fixtures; runbook
  section for the half-released repair.

Test strategy note: no fake Talos API is needed anywhere in this component — C4 talks only to the
Kubernetes API. envtest is the load-bearing tier because the concurrency design (server-side
selector transitions, resourceVersion conflicts) is exactly what fakes fake badly.

## Non-goals

- No action on claimed hardware, ever — the predicate is the contract.
- No BMC or power actions (rufio and CAPT own power; C4 must work on BMC-less hardware too).
- No disk wipe or Talos API calls — C3's reset covers the graceful path; the next provision's
  `install.wipe` covers the residual.
- No touching C1's `classify.tinkerbell.org/*` labels or `talos.tinkerbell.org/*` annotations:
  classification traits describe the physical machine and remain valid across claims.
- No deletion of `Hardware`, Workflow, Template, or rufio Job objects (CAPT and the tinkerbell
  stack own their lifecycles; external-mode rufio job orphans are an explicit charter non-goal).
- No secret rotation or revocation: scrubbing bounds future exposure of the machineconfig via
  tootles; it cannot un-leak secrets already fetched. Rotating cluster credentials after a
  suspected leak is an operational procedure outside this repo.
- No repair of half-released hardware (surfaced, not fixed — ambiguity is not actionable).

## Open questions

- Should `metadata.instance.userdata` (the legacy in-metadata twin of `spec.userData`, which
  terraform initializes to `""` and nothing serves today) be cleared as well for symmetry? Cheap
  to add to `Scrub`; deferred until any consumer appears.
- Should the half-released anomaly gain an opt-in auto-repair after a grace period (e.g.
  `--repair-half-released-after=24h` completing the release)? Deferred: no occurrence has been
  observed, and silent repair hides operator error.
- Multi-namespace Hardware (external-Tinkerbell mode) would require a ClusterRole and
  multi-namespace cache config; P6 pins this environment to the `tinkerbell` namespace, so the
  namespace-scoped shape stands until that precondition changes.
- Should the scrub additionally verify no non-terminal Workflow references the hardware before
  acting? Today the only candidates are auto-enrollment Workflows on never-claimed hardware
  (excluded by the `U` conjunct); revisit if a new Workflow producer targeting previously-claimed
  hardware appears.
