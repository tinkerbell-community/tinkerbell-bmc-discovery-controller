# C0: Discovery Controller Field-Ownership Migration

Status: Proposed — 2026-09-01

The existing tinkerbell-bmc-discovery-controller rewrites the entire `spec` of every
Hardware it manages once per resync interval, erasing fields written by other
controllers — most damagingly CAPT's `spec.userData` (the secret-bearing Talos machine
config), the future C2 component's `spec.metadata.instance.operating_system`, and the
tink workflow controller's `netboot.allowPXE=false` disarm. This document specifies the
in-scope migration of the discovery controller's upserts to server-side apply (SSA)
with the field manager `tinkerbell-bmc-discovery-controller`, asserting only the fields
discovery owns, so that foreign writers are never clobbered. It is also the repo-wide
reference for the SSA field-ownership discipline that every other component (C1–C5)
follows: one named field manager per component, sparse apply configurations limited to
owned fields, and `ForceOwnership` only over a component's own field set.

## Purpose and scope

In scope:

- Migrate the Hardware, bmc `Machine`, and auth `Secret` upserts in `internal/sync`
  from `controllerutil.CreateOrUpdate` (full-spec `Update`) to create-via-`POST` plus
  update-via-SSA-`Apply`.
- Establish the field manager name `tinkerbell-bmc-discovery-controller` and enumerate
  the exact field set discovery asserts.
- Change `netboot.allowPXE`/`allowWorkflow` handling to create-time-only authorship.
- Define the migration path for Hardware objects that already exist with legacy
  `Update`-operation field ownership, including corrupted objects.
- Preserve the managed-by adoption guard semantics exactly.

Out of scope: everything else about the discovery controller (mDNS browsing, credential
pivoting, inventory collection, naming, deletion semantics) is unchanged. No new
component is created; this is a modification of the existing binary.

## Context

This is charter work item C0. Relevant governing material in `docs/architecture.md`:
the Ownership matrix (this doc is its source of truth for discovery-owned Hardware
fields), Naming standard (SSA field manager identity == component name — C0
establishes the convention), and Precondition P4 (terraform must stop co-writing
component-owned Hardware fields and must not create duplicate Hardware for machines
discovery also creates; C0 does not fix terraform-created objects — the adoption guard
keeps discovery away from them entirely). C2 (`talos-os-metadata`) and C4
(`tinkerbell-hardware-janitor`) are broken by design until C0 lands, and CAPT is
actively corrupted hourly today, which is why C0 precedes C2 and C4 in rollout order
(and should land as early as possible to stop the ongoing CAPT userData corruption);
C1, C3, and C5 have no dependency on it.

## Current behavior and defect

### Mechanism of the defect

The sync path upserts each resource through `controllerutil.CreateOrUpdate` with a
mutate function that rebuilds the whole spec from scratch:

- `internal/sync/syncer.go:84-93` — `hardware.Spec = DesiredHardwareSpec(dev, ...)`
  inside the mutate closure; `syncer.go:100-124` (`upsert`) runs that closure on every
  sync, for existing objects too. The managed-by guard at `syncer.go:104` skips only
  objects *lacking* `discovery.tinkerbell.org/managed-by`; discovery-created Hardware
  that CAPT later claims still carries the label, so it is rewritten forever.
- `internal/sync/mapping.go:69-111` — `DesiredHardwareSpec` never sets
  `spec.userData` (`tinkv1.HardwareSpec.UserData` stays `nil`), never sets
  `spec.metadata.instance.operating_system` (`MetadataInstance` is built with only
  `ID` and `Hostname`, mapping.go:84-87), and always sets
  `Netboot{AllowPXE: ptr.To(true), AllowWorkflow: ptr.To(true)}` on the primary
  interface (mapping.go:96-104).
- `cmd/main.go:80` — `--resync-interval` defaults to `time.Hour`;
  `internal/controller/worker.go:74-101` re-enqueues every known endpoint on that
  ticker. So the full-spec rewrite lands on every managed Hardware roughly hourly,
  plus on every mDNS re-observation.

Because the write is a plain `Update` carrying the complete spec, it does not matter
what field managers other writers used: an `Update` replaces the object's spec
wholesale and takes ownership of everything it changes. SSA field management on the
*other* writers' side cannot defend against it — which is why C0 must change the
discovery controller itself.

### Concrete corruption scenarios

1. **CAPT userData wipe.** When CAPT claims Hardware for a `TinkerbellMachine`, it
   writes the rendered bootstrap cloud-config into `spec.userData`
   (cluster-api-provider-tinkerbell `controller/machine/hardware.go:169-185`,
   `ensureHardwareUserData`). Tootles serves that field as EC2 user-data; it is the
   machine's Talos config source. The next discovery resync sets `spec` to
   `DesiredHardwareSpec`, whose `UserData` is `nil`, deleting the field. A wipe
   landing between CAPT's write and the machine's first config fetch bricks the
   install. Worse, once the machine is provisioned CAPT's short-circuit never
   revisits the Hardware, so the field is never restored: re-installs, wipes, and any
   consumer of the metadata endpoint permanently lose the config until a human
   intervenes.
2. **C2 os-metadata wipe.** C2's whole contract is writing
   `spec.metadata.instance.operating_system` on claimed Hardware so workflow templates
   and tootles see resolved image identity. `DesiredHardwareSpec` rebuilds
   `Metadata.Instance` with only `ID` and `Hostname`, so every resync deletes C2's
   write. The result is a perpetual write-fight in which C2's admission-webhook
   invariant ("os metadata present before a Workflow renders") is false on the live
   object for up to an hour at a time. C2 cannot ship until this is fixed.
3. **tink-controller allowPXE re-arm.** The tink workflow controller disarms PXE after
   a workflow completes by setting `netboot.allowPXE=false` on every interface
   (tinkerbell `tink/controller/internal/workflow/hardware.go:36-96` `setAllowPXE`,
   driven by `toggleHardware` at hardware.go:98-166). Discovery's resync re-asserts
   `AllowPXE: ptr.To(true)` (mapping.go:99), re-arming PXE on provisioned machines and
   violating tink's provisioning state machine. Unlike the first two scenarios this is
   a two-writer conflict between two controllers that both exist **today**.

The same mechanism also deletes any other foreign spec field (`vendorData`,
`references`, `resources`, `metadata.instance.state`), though the three above are the
ones with known writers.

## Contracts

### Reads

| Object | Field(s) | Owner / source |
| --- | --- | --- |
| `Secret` `<credentials-secret>` (flag, default `bmc-discovery-credentials`) | `data.username`, `data.password` | Operator/user |
| `tinkerbell.org/Hardware` `<name>` (live copy before apply) | `metadata.labels["discovery.tinkerbell.org/managed-by"]` (guard), `metadata.resourceVersion` (precondition), `spec.interfaces[0].netboot` (carry-forward) | Mixed: discovery, tink-controller |
| `bmc.tinkerbell.org/Machine` `<name>` (live copy) | guard label, `resourceVersion` | discovery |
| `Secret` `<name>-bmc-auth` (live copy) | guard label, `resourceVersion` | discovery |
| mDNS DNS-SD advertisements, BMC Redfish inventory | (not API objects) | network |

### Writes

All writes use SSA field manager `tinkerbell-bmc-discovery-controller` (`==`
`ManagedByValue`, mapping.go:21). "Create-only" fields are authored at `POST` create
and never changed by discovery afterward (values carried forward verbatim on update
applies — see Design).

| Object | Field | Mode |
| --- | --- | --- |
| `Hardware` | `spec.agentID` | asserted always |
| `Hardware` | `spec.auto.enrollmentEnabled` | asserted always |
| `Hardware` | `spec.bmcRef` (`apiGroup`, `kind`, `name`) | asserted always |
| `Hardware` | `spec.metadata.manufacturer.slug` | asserted always |
| `Hardware` | `spec.metadata.instance.id` | asserted always |
| `Hardware` | `spec.metadata.instance.hostname` | asserted always |
| `Hardware` | `spec.metadata.facility.facility_code` (when `--facility-code` non-empty) | asserted always |
| `Hardware` | `spec.interfaces[0].dhcp.mac`, `spec.interfaces[0].dhcp.hostname` | asserted always |
| `Hardware` | `spec.interfaces[0].netboot.allowPXE`, `.allowWorkflow` | create-only (carried forward thereafter) |
| `Hardware` | `spec.disks[].device` | asserted always |
| `bmc Machine` | `spec.connection.*` (host, port, authSecretRef, insecureTLS, providerOptions) | asserted always |
| `Secret` `<name>-bmc-auth` | `data.username`, `data.password` | asserted always |

Fields discovery must NEVER include in an apply configuration: `spec.userData`
(CAPT), `spec.vendorData`, `spec.metadata.instance.operating_system` and
`spec.metadata.instance.state` (C2 while claimed, C4 on release), `spec.references`,
`spec.tinkVersion`, `spec.resources`, any label or annotation key outside the list
below (C1's `classify.tinkerbell.org/*` and `talos.tinkerbell.org/*`, CAPT's
`v1alpha1.tinkerbell.org/ownerName|ownerNamespace`, rufio's
`tinkerbell.org/refresh-inventory`).

Labels and annotations owned (on all three resources; maps are SSA-granular, so only
these keys are asserted):

| Key | Value |
| --- | --- |
| `discovery.tinkerbell.org/managed-by` (label) | `tinkerbell-bmc-discovery-controller` |
| `discovery.tinkerbell.org/last-seen` (annotation) | RFC3339 sync time |
| `discovery.tinkerbell.org/mdns-instance` (annotation) | mDNS instance name |
| `discovery.tinkerbell.org/mdns-service` (annotation) | DNS-SD service type |

Conditions: none (discovery writes no status on any resource). Events: none emitted
today; unchanged.

## Design

### Why SSA, and why the `interfaces` list is special

SSA scopes each write to the fields present in the apply configuration; fields owned
by other managers and absent from the configuration are untouched. For granular
structures this solves the defect outright: `spec.userData` and
`spec.metadata.instance.operating_system` simply never appear in discovery's apply
configuration again, so scenarios 1 and 2 vanish.

`spec.interfaces` needs more care. The Hardware CRD declares no
`x-kubernetes-list-type` on `interfaces` (tinkerbell/api v0.25.0
`v1alpha1/tinkerbell/hardware.go:62-63` — no `listType`/`listMapKey` markers
anywhere in the file), so under SSA the list is **atomic**: whoever applies it owns
and replaces the entire list, including `netboot` values inside items. Discovery must
keep asserting the list (it owns `dhcp.mac`/`dhcp.hostname` and must propagate MAC or
hostname changes), so it cannot simply omit `netboot` — omission would delete
tink-controller's `allowPXE=false` just as thoroughly as today's bug.

> **Decision — netboot carry-forward.** On update applies, discovery serializes the
> *live* `netboot` struct of the existing primary interface verbatim into its apply
> configuration (ownership without authorship); on create, and for a Hardware that has
> no interfaces yet, it authors the defaults `allowPXE: true`, `allowWorkflow: true`.
> The guarantee C0 provides is therefore "discovery never *changes* `netboot` after
> create", which is the property the tink state machine needs. The cleaner fix —
> `listType=map` upstream — is unavailable because `listMapKey` must name a scalar
> field directly on the list item and the natural key (`dhcp.mac`) is nested; recorded
> as an open question.

The carry-forward read races with a concurrent tink-controller write, so update
applies carry an optimistic-concurrency precondition: the sparse object's
`metadata.resourceVersion` is set from the `Get`, which makes the Apply fail with a
`Conflict` if anything changed in between. Conflicts return an error to the worker,
which already retries with rate-limited backoff (worker.go:105-119).

### Create path vs update path

> **Decision — `POST` create, SSA update.** New objects are created with a plain
> `client.Create` (with `client.FieldOwner(FieldManager)`), not with an Apply.
> Rationale: (a) Apply is an upsert, so a `Get`→`NotFound`→`Apply` sequence racing a
> concurrent terraform/hand creation would silently adopt and mutate a foreign object,
> defeating the adoption guard; `Create` fails with `AlreadyExists` instead, and the
> retry then sees an unlabeled object and skips it. (b) It gives a natural home to
> create-only authorship of `netboot`. The create request contains exactly the fields
> in the Writes table (it is the same sparse desired object), so the resulting
> `Update`-operation managed-fields entry covers only discovery's field set and is
> absorbed by the first Apply.

Per-resource flow (identical for Hardware, Machine, Secret):

1. Build the sparse desired object: `TypeMeta` set explicitly (required — the Apply
   patch body must carry `apiVersion`/`kind`, and typed objects do not get them filled
   in by the client), name/namespace, the owned labels/annotations, and only the spec
   fields from the Writes table.
2. `Get` the live object.
   - `NotFound` → complete the desired object with create-time `netboot` defaults →
     `Create`. On `AlreadyExists`: return the error, let the worker requeue.
   - Found without `discovery.tinkerbell.org/managed-by` → log and skip (`nil`),
     exactly today's guard semantics (syncer.go:104,127-129). Terraform-created and
     hand-provisioned resources are never touched, created, or adopted.
   - Found and labeled → continue.
3. For Hardware: copy the live primary interface's `netboot` into the desired
   object's interface entry (carry-forward). Other resources have no carried fields.
4. Set `desired.ResourceVersion = live.ResourceVersion`.
5. `Patch(ctx, desired, client.Apply, client.FieldOwner(FieldManager),
   client.ForceOwnership)`.
6. On `Conflict` (resourceVersion moved): return the error → rate-limited requeue.

> **Decision — `ForceOwnership` always.** Kubernetes guidance is that controllers
> force conflicts on fields they own, and force is scoped to fields present in the
> apply configuration — foreign fields are structurally unreachable because they are
> never serialized. Force is also what makes the legacy-ownership migration and the
> tink-controller ownership ping-pong (below) deterministic. The safety boundary is
> the sparse builder, not conflict arbitration.

### Interaction with the managed-by adoption guard

The guard's semantics are preserved bit-for-bit: an existing object without the
managed-by label is never modified. Two SSA-specific hazards are addressed by the
design above: Apply-as-upsert cannot adopt foreign objects (create is `POST`), and
the guard check rides the same `Get` the carry-forward and precondition need, so the
guard is evaluated against the exact revision the Apply is conditioned on — a label
stripped between check and write surfaces as a `Conflict`, not a corruption.

### Ownership migration for existing objects

Objects created by the current code carry managed-fields entries of operation
`Update` under the manager name the API server inferred from the client user agent
(controller-runtime sets none explicitly; typically `manager`). Verify on a live
cluster with `kubectl get hardware <name> -o yaml --show-managed-fields`.

First Apply over such an object behaves as follows (SSA co-ownership/transfer
semantics):

- Asserted fields whose applied value equals the live value transfer to the
  `tinkerbell-bmc-discovery-controller` applier without conflict.
- Asserted fields whose value differs (e.g. a changed disk list) would raise a
  conflict against the legacy `Update` manager — `ForceOwnership` resolves it in
  discovery's favor, which is correct because every asserted field is discovery's by
  contract.
- Fields the legacy manager owns that the new configuration does not assert remain on
  the object, still listed under the legacy entry; they are inert (an applier only
  removes fields *it* owns by omitting them; it never removes other managers' fields).
  In practice the legacy entry covers exactly the old `DesiredHardwareSpec` output, so
  after the first Apply it is empty or near-empty. No cleanup pass is needed.
- Foreign managers (CAPT's patch-helper manager on `spec.userData`, tink-controller's
  on `interfaces`) are unaffected except for `interfaces`: ownership of the atomic
  list ping-pongs by design — tink's `Update` takes the list when it disarms PXE,
  discovery's next Apply takes it back with the disarmed value carried forward. Values
  are stable; only the managed-fields bookkeeping oscillates.

Migration is therefore automatic and per-object on first resync after the upgrade; no
migration job, annotation, or version gate is required.

Already-corrupted objects are not healed by C0: a claimed Hardware whose `userData`
was wiped before the upgrade stays wiped (CAPT's provisioned short-circuit never
rewrites it). Rollout includes a fleet audit (Deployment section) so operators can
find and remediate those by hand (re-provision, or restore `spec.userData` from the
machine's bootstrap secret).

### Secret and bmc Machine upserts

> **Decision — same treatment for all three resources.** The auth `Secret` and bmc
> `Machine` currently have no second writer (rufio writes only `Machine.status`), so
> full-spec updates are not corrupting them today. They migrate to the identical
> create/apply flow anyway: it is one shared helper either way, it removes the
> repo's last `CreateOrUpdate` full-object write so the SSA discipline has no
> exceptions to explain, and it future-proofs the `Machine` against later spec
> writers. `Secret.data` and label/annotation maps are SSA-granular, so key-level
> ownership falls out for free; a rotated password propagates, and a data key
> discovery stops asserting is removed (discovery owns it).

### Idempotency and write churn

Builders are pure functions of `(endpoint, inventory, options, live-netboot)`; the
same inputs always produce the same apply configuration, and Apply itself is
idempotent. The `last-seen` annotation changes every cycle, so applies are never full
no-ops — matching today's behavior (`CreateOrUpdate` also wrote hourly). Spec-level
stability means `metadata.generation` does not churn when inventory is unchanged.
Reducing `last-seen` write frequency is noted as an open question, not in scope.

## Failure modes and degradation

| Condition | Behavior |
| --- | --- |
| Apply returns `Conflict` (resourceVersion precondition) | Error → worker requeues with rate-limited backoff; next attempt re-reads and re-carries. Livelock is not a realistic risk (tink writes `allowPXE` once per workflow transition). |
| `Create` returns `AlreadyExists` (creation race) | Requeue; next pass sees the object — labeled: apply; unlabeled: guard skip. Never adopts the foreign object. |
| Live object unlabeled (terraform/hand-created) | Skipped, logged at warn — unchanged behavior. Duplicate-creator hazards are P4's (terraform) problem; discovery neither fixes nor worsens them. |
| API server unavailable / `Get` fails | Error → requeue; discovery holds no local state that can go stale destructively. |
| CAPT / C2 / tink-controller absent | Irrelevant: discovery asserts only its own fields and reads none of theirs except live `netboot`, whose absence yields create defaults. Discovery deploys standalone exactly as today. |
| Discovery down | No Hardware updates (MAC/disk drift not propagated); nothing corrupts. Fail-open. |
| Rollback to a pre-C0 image | **Reintroduces the full-spec wipe.** The chart pins the image tag; operators must treat pre-C0 versions as recalled once any of CAPT/C2/C4 is active. Called out in release notes. |

## RBAC

No changes. SSA Apply is the `patch` verb, and the existing namespace-scoped Role
already grants `get,list,watch,create,update,patch` on `secrets`,
`bmc.tinkerbell.org/machines`, and `tinkerbell.org/hardware`
(helm/tinkerbell-bmc-discovery-controller/templates/rbac.yaml:1-40). The migrated sync
path uses `get` + `create` + `patch`; `update` becomes unused by it but stays granted —
harmless, and dropping it would break a rollback to a pre-C0 image.

## Deployment

Chart shape is unchanged: same Deployment, flags, values, RBAC; no Service, no TLS.
No new flags — the legacy behavior is a defect, not a mode, so there is no
compatibility toggle (Decision). Ship as a normal appVersion bump with an independent
chart version bump per repo convention.

Rollout plan:

1. Deploy C0 before C2 or C4 are enabled anywhere, and as early as possible relative
   to CAPT activity (every hour of delay is another wipe window on claimed Hardware).
2. Post-upgrade verification on one managed Hardware:
   `kubectl get hardware <name> -o yaml --show-managed-fields` — expect an
   `Apply`-operation entry for manager `tinkerbell-bmc-discovery-controller` covering
   exactly the Writes-table fields.
3. Fleet audit for pre-existing corruption (claimed Hardware missing userData):

   ```sh
   kubectl get hardware -n tink -o json | jq -r '
     .items[]
     | select(.metadata.labels["v1alpha1.tinkerbell.org/ownerName"] != null)
     | select(.spec.userData == null)
     | .metadata.name'
   ```

   Each hit needs manual remediation (restore `spec.userData` from the machine's
   bootstrap data secret, or re-provision); C0 only stops further damage. The owner
   label is CAPT's claim marker (cluster-api-provider-tinkerbell
   `controller/machine/hardware.go:27`).

## Implementation plan

All work stays in `internal/sync` plus tests; `cmd/main.go` and the worker are
untouched.

Key types and functions:

```go
// mapping.go
// FieldManager is the SSA field manager for every write this controller
// makes. It deliberately equals ManagedByValue: manager identity ==
// component name (repo naming standard).
const FieldManager = ManagedByValue

// DesiredHardware returns the sparse apply configuration for the Hardware:
// TypeMeta, name/namespace, owned labels/annotations placeholders, and only
// discovery-owned spec fields. live carries netboot forward; nil live means
// create (netboot defaults allowPXE/allowWorkflow true).
func DesiredHardware(dev *common.Device, opts HardwareOptions, live *tinkv1.Hardware) *tinkv1.Hardware

// netbootFor implements the carry-forward rule.
func netbootFor(live *tinkv1.Hardware) *tinkv1.Netboot

// DesiredMachine and DesiredAuthSecret follow the same sparse-object shape.
func DesiredMachine(ep mdns.Endpoint, name string, insecureTLS bool, authRef corev1.SecretReference) *bmcv1.Machine
func DesiredAuthSecret(name, namespace string, creds inventory.Credentials) *corev1.Secret
```

```go
// syncer.go
// applyManaged gets the live object into `live` (same concrete type as
// desired), enforces the adoption guard, and either Creates (not found) or
// SSA-Applies (found+labeled) the desired sparse object with FieldManager,
// ForceOwnership, and a resourceVersion precondition. Returns errUnmanaged
// handling identical to today (logged, nil).
func (s *Syncer) applyManaged(ctx context.Context, kind string, live, desired client.Object, ep mdns.Endpoint) error
```

`Syncer.Sync` keeps its signature and flow, calling `applyManaged` three times; the
old `upsert` and the spec-returning `DesiredHardwareSpec`/`DesiredMachineSpec` are
removed (builders now return whole sparse objects because labels, annotations,
TypeMeta, and resourceVersion are part of the apply payload).

Implementation gotchas to encode in code comments and tests:

- Sparse objects must set `TypeMeta` explicitly; `client.Apply` marshals the object
  as the patch body and the server rejects a patch without `apiVersion`/`kind`.
- Never reuse the `Get` result as the apply object (it carries `managedFields`,
  which is illegal in an apply payload); always build fresh.
- `Auto.EnrollmentEnabled` is a non-pointer bool with `omitempty` (tinkerbell/api
  hardware.go:859-863): `false` serializes as `auto: {}`, i.e. the field is *removed*
  when the flag is off. Absent equals false for all consumers; document, don't fight.
- `Netboot.AllowPXE`/`AllowWorkflow` are `*bool`, so explicit `false` carry-forward
  round-trips correctly.

Test strategy:

- Unit (existing table-driven style, no cluster): builder purity and sparseness —
  assert via `json.Marshal` that forbidden fields (`userData`,
  `operating_system`, `state`, `vendorData`, `references`, `resources`) never appear
  in any apply payload, and that carry-forward reproduces live `netboot` exactly.
- envtest (new to the repo; add `setup-envtest` + a `make test-envtest` target):
  managed-fields semantics are only trustworthy against a real API server — the
  controller-runtime fake client's SSA emulation has diverged from apiserver behavior
  historically, so ownership assertions must not rely on it. Scenarios:
  1. Foreign-field survival: seed Hardware, simulate CAPT (`spec.userData` via
     optimistic-lock patch), C2 (`operating_system` via SSA manager
     `talos-os-metadata`), and tink-controller (`allowPXE=false` via `Update`); run
     `Sync` twice; assert all three survive and `allowPXE` stays `false`.
  2. Legacy adoption: create Hardware via full-spec `Update` under manager `manager`;
     run `Sync`; assert the Apply entry owns exactly the Writes-table field set and
     foreign fields are untouched.
  3. Guard: unlabeled Hardware/Machine/Secret are never modified and never adopted,
     including through the create race (`AlreadyExists` → skip on retry).
  4. Precondition: bump the object between `Get` and Apply (interceptor) → `Conflict`
     surfaces as an error; a rerun succeeds and preserves the interleaved write.
  5. Create path: fresh object gets `allowPXE`/`allowWorkflow` true, label, and the
     three annotations, with manager `tinkerbell-bmc-discovery-controller`.
  6. Secret rotation: changed password propagates; removed data key is pruned.

Milestones:

- M1: sparse builders (`DesiredHardware`/`DesiredMachine`/`DesiredAuthSecret`,
  `netbootFor`) + unit tests, old builders still wired.
- M2: `applyManaged` replaces `upsert`; guard, precondition, create path; worker-level
  behavior tests updated.
- M3: envtest suite (scenarios 1–6) + CI wiring (`setup-envtest` in the test job).
- M4: release — chart/appVersion bump, release notes flagging the rollback hazard and
  the fleet audit; update `docs/architecture.md` ownership matrix reference.

## Non-goals

- No conversion of the poll-loop worker into a watch-based `Reconciler`, and no
  deletion/garbage-collection semantics (mDNS disappearance still never deletes).
- No healing of already-wiped `userData`/os metadata (manual remediation; audit
  provided).
- No writes to, or defense of, terraform-created Hardware (P4's boundary), and no
  ownership of any C1/C2/C4 field.
- No upstream tinkerbell CRD changes (e.g. `listType` markers) as part of C0.
- No multi-interface Hardware modeling; discovery still records only the primary MAC.

## Open questions

1. Upstream `listType=map` for `Hardware.spec.interfaces` would let discovery drop the
   netboot carry-forward entirely, but `listMapKey` cannot reference the nested
   `dhcp.mac`; it would require a schema change (key field hoisted onto `Interface`).
   Worth filing upstream; C0 does not wait for it.
2. Should `last-seen` updates be decoupled from spec applies (e.g. only written when
   another field changed, or rounded to the resync interval) to cut hourly write
   churn on large fleets? Deferred; behavior-preserving for now.
3. If a future tinkerbell release adds spec writers to bmc `Machine` (e.g. rufio
   feature negotiation), does the carry-forward pattern generalize, or should
   `Machine.spec.connection` be declared wholly discovery-owned in the architecture
   ownership matrix? Currently declared wholly discovery-owned.
