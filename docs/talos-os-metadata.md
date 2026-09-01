# talos-os-metadata

Status: Proposed — 2026-09-01

talos-os-metadata (C2) mirrors the per-machine image identity that the CAPT fork resolves —
`TinkerbellMachine.status.{schematicID,installerImage,diskImageURL}` — into
`Hardware.spec.metadata.instance.operating_system` before Tinkerbell renders the machine's Workflow,
and enforces that ordering with an opt-in ValidatingAdmissionWebhook on Workflow CREATE. It is a pure
mirror: it never computes schematics, never talks to the Image Factory, and never clears what it
wrote (clearing on hardware release is C4's job — a state-disjoint handoff). The webhook is scoped to
CAPT-owned workflows only and is deliberately fail-open when schematic resolution has not produced a
result, so a Factory outage or a missing `talosVersion` degrades visibly instead of deadlocking
provisioning cluster-wide.

## Purpose and scope

Nothing in the deployed stack writes `Hardware.spec.metadata.instance.operating_system` after
creation: CAPT neither sets nor reads it (grep over the CAPT fork finds zero controller references),
and the tinkerbell stack only consumes it — the deployed workflow template composes its raw-image
`IMG_URL` from it (tfc/cluster-bootstrap `modules/cluster/templates/template-data.yaml:29`) and
tootles serves it verbatim on the EC2 `meta-data/operating-system/*` endpoints
(tinkerbell `tootles/internal/frontend/ec2/routes.go:11-117`). Today the field is written once by
terraform at Hardware creation and then goes stale forever. C2 gives the field a live, single owner
sourced from the resolution CAPT already performs (F2), and realizes the brief's "populate os
metadata before templates/workflows run" blocking hook as admission — the only workable enforcement
point, because Workflows render exactly once at tink-controller's first reconcile when
`status.state == ""` (tinkerbell `tink/controller/internal/workflow/reconciler.go:146-159`), and
denying CREATE fully prevents that render (F5).

In scope: the mirror controller, the admission webhook, their shared field mapping, the repo's first
TLS machinery (cert-manager), and the chart. Out of scope: schematic computation (CAPT, F2),
clearing metadata on release (C4), classification inputs (C1), the workflow template migration to
`{{ .diskImageURL }}` (cross-repo recommendation, P7).

## Context

Governing facts that shape this component (see `docs/architecture.md`, Governing facts and
Preconditions): F2 (CAPT resolves the schematic and publishes the status trio; C2 must not
re-resolve), F5 (render-once semantics; deny-at-CREATE is the only gate; CAPT retries denied creates
with clean exponential backoff and does not watch Hardware), F8/F9 (no Runtime SDK machinery;
cert-manager is available; this is the repo's first TLS machinery, and this component requires a
ClusterRole — the repo's precedent is namespace-scoped Roles). Preconditions that
gate C2's usefulness: P1 (terraform must pass a pinned `talosVersion`, otherwise the trio is never
produced and C2 idles fail-open), P2 (CAPT schematics must become bootable-raw-image capable before
C2's mirror replaces terraform-written metadata as the template's source), P4 (terraform must stop
re-asserting this field), and C0 (the discovery controller's full-spec rewrite must move to SSA, or
it erases C2's writes hourly on discovery-managed Hardware). P7 pins which artifact name the
template consumes.

## Contracts

### Reads

| Object | Field | Owner |
| --- | --- | --- |
| `TinkerbellMachine` (`infrastructure.cluster.x-k8s.io/v1beta2`) | `status.schematicID`, `status.installerImage`, `status.diskImageURL` | CAPT (capt `controller/machine/schematic.go:63-65`) |
| `TinkerbellMachine` | `spec.hardwareName`, `status.targetNamespace` | CAPT |
| `Hardware` (`tinkerbell.org/v1alpha1`) | labels `v1alpha1.tinkerbell.org/ownerName`, `v1alpha1.tinkerbell.org/ownerNamespace` | CAPT (capt `controller/machine/hardware.go:27-31`) |
| `Hardware` | `spec.metadata.instance.operating_system` (current value, for comparison) | C2 itself |
| `Hardware` | label `classify.tinkerbell.org/classified` (optional check only) | C1 |
| `Workflow` (admission request payload only) | labels `capt.tinkerbell.org/machine-name`, `capt.tinkerbell.org/machine-namespace`; `spec.hardwareRef` | CAPT (capt `controller/machine/tinkerbellmachine.go:53-56`, `workflow.go:48`) |
| `Machine` (`cluster.x-k8s.io`, read as unstructured) | `spec.bootstrap.configRef` | CAPI core |
| `TalosConfig` (`bootstrap.cluster.x-k8s.io`, read as unstructured) | `spec.talosVersion` | CABPT / user (diagnosis only) |

### Writes

| Object | Field | SSA field manager |
| --- | --- | --- |
| `Hardware` (`tinkerbell.org/v1alpha1`) | `spec.metadata.instance.operating_system` `{slug, distro, version, image_tag, os_slug}` — the whole block, nothing else in spec | `talos-os-metadata` |
| `Event` (core v1), on `TinkerbellMachine` | reasons owned: `OSMetadataMirrored`, `SchematicUnresolved`, `TalosVersionMissing`, `IncoherentStatusTrio`, `MirrorConflict` | n/a |

Labels/annotations/conditions owned: none on API objects in v1. The `osmeta.tinkerbell.org` domain
is reserved for this component per the naming standard but deliberately unused — degradation is
surfaced as Events plus Prometheus metrics (see the Decision under Failure modes). C2 writes no
conditions anywhere: `TinkerbellMachine.status.conditions` is CAPT's, and the tinkerbell `Hardware`
type has no third-party-writable conditions slot.

## Design

### Mirror reconcile flow

The controller reconciles `TinkerbellMachine` objects (the source of truth for the trio) and treats
`Hardware` as a secondary trigger so that an external erase of the mirror (terraform re-apply,
pre-C0-fix discovery resync) is repaired promptly.

1. Fetch the `TinkerbellMachine`. Gone or deleting: do nothing. C2 has no finalizers and no cleanup
   duties — see the handoff to C4 below.
2. Read the status trio. If `status.schematicID` is empty, diagnose why (best effort, for the
   degradation signal only): walk the owner `Machine`'s `spec.bootstrap.configRef` to the
   `TalosConfig` and read `spec.talosVersion` as unstructured — the exact pattern CAPT itself uses
   (capt `controller/machine/schematic.go:77-112`). No valid full `vX.Y.Z` version → emit
   `TalosVersionMissing` (P1 unmet, resolution is not expected); valid version but empty trio → emit
   `SchematicUnresolved` (resolution expected, pending or failing — Factory outage, registrar
   error). Either way, write nothing: C2 never invents image identity. Return without requeue; the
   watch on `TinkerbellMachine` retriggers when CAPT updates status.
3. Trio present: locate the claimed Hardware by `spec.hardwareName` in `status.targetNamespace`
   (falling back to the machine's namespace when unset, matching CAPT's default). Missing
   `hardwareName` cannot normally coexist with a resolved trio (CAPT resolves against the claimed
   Hardware), so treat it as transient and requeue.
4. Verify the Hardware is still claimed by this machine: labels
   `v1alpha1.tinkerbell.org/ownerName == machine.Name` and
   `.../ownerNamespace == machine.Namespace`. If not, stop — C2 must never act on released
   Hardware.
5. Compute the desired `operating_system` block from the trio (mapping below). If the trio is
   internally incoherent — the `installerImage` tag disagrees with the version parsed from
   `diskImageURL` — CAPT is mid-update; emit `IncoherentStatusTrio` and requeue shortly rather than
   mirroring a chimera.
6. If the current block already equals the desired block, done. Otherwise SSA-Apply a minimal
   object asserting only `spec.metadata.instance.operating_system`, with field manager
   `talos-os-metadata`, `ForceOwnership`, and the read object's `resourceVersion` as an optimistic
   precondition. A conflict means the Hardware changed between the claimed-check and the write
   (release/re-claim race) — requeue and re-evaluate from step 3. Emit `OSMetadataMirrored` on a
   successful first write or a value change.

Idempotency: the desired block is a pure function of the status trio; the Apply is a no-op when
nothing changed; the same reconcile can run any number of times with the same result.

> **Decision — SSA Apply with ForceOwnership plus a resourceVersion precondition.** The charter
> designates C2 the sole writer of this field, so a manager conflict can only mean a misbehaving
> co-writer (terraform before P4, discovery before C0); forcing ownership keeps the mirror correct
> and the conflict is surfaced via the `osmeta_mirror_conflicts_total` metric and `MirrorConflict`
> events rather than by wedging. The resourceVersion precondition is what makes
> "verify-claimed-then-write" atomic against the release path — the same guard discipline C4 uses.
> The Apply payload is built as `unstructured.Unstructured` containing exactly the asserted paths,
> because a typed Apply of the full `Hardware` struct would co-assert unrelated zero-valued fields.

### Field mapping

Desired `Hardware.spec.metadata.instance.operating_system` (type
`MetadataInstanceOperatingSystem`, tinkerbell `api/v1alpha1/tinkerbell/hardware.go:335-341`):

| Field | Value | Derivation |
| --- | --- | --- |
| `slug` | `status.schematicID` | verbatim |
| `distro` | `talos` | constant |
| `version` | `<ver>` | the version path segment of `status.diskImageURL` (`<factory>/image/<id>/<ver>/metal-<arch>.raw.xz`, capt `pkg/schematic/schematic.go:199-202`); full Talos version with the `v` prefix, e.g. `v1.13.9` |
| `image_tag` | `<ver>` | same value — matches the terraform convention (`modules/data-lookup/modules/images/locals.tf:39-47`) |
| `os_slug` | `talos-<ver>-<arch>` | `<arch>` parsed from the `metal-<arch>.raw.xz` filename of `status.diskImageURL` |

`os_slug` exists so the deployed template can recover the architecture:
`OsSlug | splitList "-" | last` (template-data.yaml:29). Pre-release versions such as `v1.14.0-rc.1`
contain dashes, but `last` still yields the architecture because arch is always the final segment.

> **Decision — the arch (and version) source that wins is `status.diskImageURL`.** Three candidate
> arch sources exist: the trio's `diskImageURL` filename, a fresh mapping of
> `Hardware.spec.interfaces[].dhcp.arch` (`aarch64`→`arm64` etc., capt
> `pkg/schematic/schematic.go` `architectureOf`, default `amd64`), and terraform's historical
> values. The `diskImageURL` wins because it *is* `signals.Architecture` as resolved by CAPT at
> schematic time (capt `controller/machine/schematic.go:52`) — re-deriving from `dhcp.arch` could
> diverge if interfaces changed after resolution, and the mirror must never disagree with the
> resolver. The same reasoning makes `diskImageURL` the version source: the charter names
> "bootstrap-ref talosVersion" as an input, and C2 does read it, but only for the degradation
> diagnosis in step 2 — during an in-place version bump (post-P3) there is a window where the
> bootstrap ref already says `v1.14.x` while the trio still carries `v1.13.x`, and mirroring the
> bootstrap-ref version against the old schematic ID would fabricate an image that was never
> resolved. The trio is atomic with itself; it is the sole value source.

### State-disjoint handoff to C4 — C2 never clears

C2 writes only while the Hardware is claimed by the sourcing machine (step 4). On release, CAPT
removes both owner labels, the provisioned annotation, and its finalizers (capt
`controller/machine/hardware.go:347-365`) — from that instant C2's write predicate can never hold
again for the departed machine, and clearing the now-stale `operating_system` block (plus instance
state and `userData`) is C4's job under its released predicate (owner labels absent ∧
`spec.userData` non-empty ∧ provisioned annotation absent). The two predicates are disjoint by
construction — claimed and released cannot both be true — so C2 and C4 can never interleave writes
on the same Hardware state. C2 must not "repair" its mirror on released Hardware even though its
field-manager entry survives until C4's clear; the owner-label check in step 4 is load-bearing, not
cosmetic. After C4 clears the field, the next claim produces a fresh trio and a fresh mirror.

### Admission webhook

A `ValidatingWebhookConfiguration` named `talos-os-metadata-webhook` with one webhook:

- Rules: `apiGroups: ["tinkerbell.org"]`, `apiVersions: ["v1alpha1"]`, `resources: ["workflows"]`,
  `operations: ["CREATE"]`, `scope: Namespaced`, `matchPolicy: Equivalent`.
- `objectSelector`: `matchExpressions: [{key: capt.tinkerbell.org/machine-name, operator: Exists}]`.
  CAPT stamps this label on the Workflow object before Create (capt
  `controller/machine/scope.go:132-150`), and tink-server auto-enrollment Workflows are unlabeled
  (F5) — so auto-enrollment is structurally excluded regardless of failure policy, at the API
  server's matching layer, before the webhook is ever called.
- `sideEffects: None`, `admissionReviewVersions: ["v1"]`, `timeoutSeconds: 10`.
- `failurePolicy: Fail` (Decision below).
- `clientConfig.service`: `talos-os-metadata` in the release namespace, path
  `/validate-tinkerbell-org-v1alpha1-workflow`, port 443 → container 9443; `caBundle` injected by
  cert-manager's cainjector via the `cert-manager.io/inject-ca-from` annotation.

Handler flow, per admission request:

1. Decode the Workflow. Resolve the owning `TinkerbellMachine` from the
   `capt.tinkerbell.org/machine-name|machine-namespace` labels and the target `Hardware` from
   `workflow.spec.hardwareRef` (set by CAPT, capt `controller/machine/workflow.go:48`) in the
   workflow's namespace, both via the manager's informer cache.
2. If the machine or the Hardware cannot be read: deny with a retriable message (cache lag; CAPT
   retries).
3. If `--require-classified-marker` is enabled and the Hardware lacks C1's
   `classify.tinkerbell.org/classified` label: deny, retriable. This check runs *before* the
   trio checks on purpose — a mirror of an extensionless schematic is perfectly "current", and only
   a deny at this point is corrective: the machine is unprovisioned, so CAPT's retry re-runs
   `reconcileSchematic`, which then unions in the extensions C1 has meanwhile annotated (capt
   `controller/machine/schematic.go:44`).
4. If the trio is empty (`status.schematicID == ""`): **admit**, attaching an admission warning
   (`schematic unresolved; workflow will provision from the template's fallback image identity`).
   No API writes from the webhook — the mirror controller observes the same state and emits the
   events.
5. Trio present: compute the desired block with the same pure function the controller uses and
   compare it to the Hardware's current `operating_system`. Equal → admit. Absent or stale → deny
   with a retriable message naming the Hardware and the field
   (`os metadata mirror not yet current for hardware tinkerbell/<name>; talos-os-metadata is
   converging — the create will be retried`).

Decision table:

| # | Classified-marker check | Status trio | Hardware mirror | Verdict |
| --- | --- | --- | --- | --- |
| 1 | disabled or marker present | present | current (equals desired) | Admit |
| 2 | disabled or marker present | present | absent or stale | Deny (retriable) |
| 3 | disabled or marker present | empty | any | Admit + warning; controller emits event |
| 4 | enabled, marker absent | any | any | Deny (retriable) |
| 5 | machine/Hardware unreadable | — | — | Deny (retriable) |

Fail-open rationale for row 3 (from the completeness critique, folded into the charter): a
fail-closed rule on an empty trio converts every resolution stall — a Factory outage, a registrar
error, and above all the *currently true* P1 state where `talosVersion` is never set and CAPT skips
resolution for every machine (capt `controller/machine/schematic.go:36-42`) — into a cluster-wide
provisioning deadlock with no self-healing path. An empty trio also has a legitimate fallback: CAPT
omits the schematic keys from `hardwareMap` when unresolved precisely so templates keep working on
their own defaults (capt `controller/machine/workflow.go:134-144`), and terraform-created Hardware
carries terraform-written os metadata. So absence of resolution degrades (visibly, via events,
warnings, and the `osmeta_degraded` gauge) instead of blocking; only *mirror lag relative to an
actual resolution* is denied, and that deny is guaranteed to converge because the denied create
leaves the trio persisted (CAPT's deferred status patch runs even on the denied path, capt
`controller/machine/scope.go:180-194,381-388`), the mirror controller writes, and the retry admits.

> **Decision — `failurePolicy: Fail`.** The charter left Ignore-vs-Fail open. Fail is correct here
> because the two failure axes are different: the *data-absent* case (row 3) is fail-open, since
> its duration is unbounded and outside the operator's control; the *enforcement-point-down* case
> is bounded by C2's own deployment health, which the operator controls, and CAPT recovers
> automatically the moment the webhook is back (exponential-backoff retry, no terminal state — F5).
> With `Ignore`, an outage of the C2 pod silently voids the only invariant the webhook exists to
> enforce, exactly when it is likeliest to be violated (the mirror controller is down too — same
> pod). The blast radius of `Fail` is narrow by construction: the `objectSelector` matches only
> CAPT-labeled workflows, so auto-enrollment and any other Workflow creators are untouched, and the
> worst case is "provisioning of new CAPI machines pauses until the pod reschedules". Running the
> webhook and the mirror in one binary is deliberate for the same reason: a reachable webhook with
> a dead mirror would deny forever, so they must share fate. The chart exposes
> `webhook.failurePolicy` for operators who weigh availability differently.

### Retry cadence caveat

CAPT does not watch Hardware — its manager watches `Machine`, `TinkerbellCluster`, `Cluster`,
`Workflow`, and rufio `Job` only (capt `controller/machine/tinkerbellmachine.go:234-284`). After a
denial there is therefore no event-driven retrigger when C2 completes the mirror write; the retry is
backoff-only from the failed reconcile. In practice the mirror write lands in well under a second
and the first few controller-runtime retries (5 ms base, doubling) admit almost immediately; but if
a denial persists (mirror blocked by an unfixed C0, RBAC error), the retry interval grows toward the
default cap (~1000 s), so recovery after a long outage can take up to ~17 minutes to be noticed. A
considered alternative — C2 stamping a nudge annotation on the `TinkerbellMachine` after each mirror
write to force an immediate CAPT reconcile — was rejected for v1: it adds a foreign-object write and
a second writer on CAPT's primary object for a latency win that only matters in already-degraded
states. The charter records backoff-only as acceptable; revisit only if real-world convergence
proves slow.

### P7 — artifact-name pinning and the hardwareMap migration

Two artifact spellings exist today: CAPT's `status.diskImageURL` ends in `.raw.xz` (capt
`pkg/schematic/schematic.go:199-202`) while the deployed template hardcodes `.raw.zst`
(template-data.yaml:29); the Factory currently serves both. Per P7 the template must consume exactly
one documented field. The recorded recommendation is to migrate templates to the `hardwareMap` keys
CAPT injects at Workflow CREATE — `{{ .diskImageURL }}` (capt
`controller/machine/workflow.go:128-146`) — instead of reconstructing the URL from Hardware os
metadata. After that migration the render path no longer reads `operating_system` at all, and this
webhook stops being load-bearing for install correctness: it remains purely an ecosystem-compat
guarantee (tootles EC2 `operating-system` endpoints, human inspection, any future Hardware-metadata
consumer). C2's mirror stays valuable either way; the webhook stays opt-in either way, and whether
enabling it is still worth recommending after a verified migration is an operator choice (see Open
questions).

## Failure modes and degradation

| Dependency state | Behavior |
| --- | --- |
| P1 unmet (`talosVersion` never set — today's state) | Trio empty for every machine. Mirror idles per machine, emits `TalosVersionMissing` once (deduped via event aggregation), `osmeta_degraded{reason="talos-version-missing"}` set. Webhook admits with warning. Provisioning proceeds exactly as before C2 existed. |
| Image Factory outage / registrar errors | CAPT's reconcile errors before Workflow create, or trio simply never lands; whichever way, C2 sees an empty trio → fail-open as above (`SchematicUnresolved`). |
| C2 pod down (webhook enabled) | Webhook unreachable → with `failurePolicy: Fail`, CAPT workflow CREATEs are rejected by the API server and retried on backoff; provisioning of new machines pauses and resumes on recovery. Existing mirrors and running nodes are unaffected. In the default mirror-only deployment (`webhook.enabled=false`), downtime only delays mirror repairs. |
| cert-manager absent / cainjector fails | The webhook config has no valid `caBundle` → TLS failure → denies under `Fail`. cert-manager is a hard dependency of the webhook (it is installed by terraform, `modules/core/modules/cert-manager`); the chart refuses to render `webhook.enabled=true` without `certificate.enabled=true`. The default `webhook.enabled=false` deployment is mirror-only and needs no cert-manager. |
| C0 unfixed (discovery full-spec rewrite) | On discovery-managed Hardware the hourly resync erases the mirror (discovery `internal/sync/syncer.go:84-124`); C2's Hardware watch repairs it within seconds, but tootles serves an empty block in the window, and a Workflow created in the window is denied until repair. C0's SSA migration is a stated prerequisite in the charter. |
| P4 unmet (terraform re-asserts os metadata) | Each `terraform apply` stomps the mirror on terraform-created Hardware; C2 forces it back (SSA ForceOwnership), emitting `MirrorConflict` and incrementing `osmeta_mirror_conflicts_total`. Churn, not corruption. |
| Hardware released mid-write | resourceVersion precondition conflicts; requeue re-evaluates the claim predicate and stops. Stale metadata on released Hardware is C4's to clear, by contract. |
| Incoherent trio (mid-update) | No write; `IncoherentStatusTrio` event; short requeue until CAPT converges. |
| External-Tinkerbell mode | Workflows and Hardware live on the external cluster, so the webhook and mirror must be deployed *there*. The current environment runs local mode; documented, not built. |

## RBAC

This component requires a ClusterRole (the repo's precedent is namespace-scoped Roles — F9; C2 must
read CAPI-namespace machines and tinkerbell-namespace Hardware across namespaces).

ClusterRole `talos-os-metadata`:

```yaml
rules:
  - apiGroups: ["tinkerbell.org"]
    resources: ["hardware"]
    verbs: ["get", "list", "watch", "patch"]
  - apiGroups: ["infrastructure.cluster.x-k8s.io"]
    resources: ["tinkerbellmachines"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["cluster.x-k8s.io"]
    resources: ["machines"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["bootstrap.cluster.x-k8s.io"]
    resources: ["talosconfigs"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
```

Namespaced Role `talos-os-metadata-leader-election` in the release namespace:
`coordination.k8s.io/leases` `get/create/update`. `patch` on `hardware` is the only write verb in
the entire component (SSA Apply is a PATCH); the webhook needs no verbs beyond the reads above
because the Workflow object arrives in the AdmissionReview payload.

## Deployment

Chart `helm/talos-os-metadata` (component = binary = chart = image name per the naming standard);
one `cmd/talos-os-metadata/main.go` binary added to `.goreleaser.yaml` `builds[]`/`dockers_v2[]` as
a sibling of `manager`, scratch image `ghcr.io/tinkerbell-community/talos-os-metadata`.

Templates: `deployment.yaml` (1 replica, RollingUpdate — no hostNetwork or host ports, unlike the
discovery controller's Recreate; ports 9443 webhook, 8080 metrics, 8081 probes; runAsNonRoot 65532),
`service.yaml` (Service `talos-os-metadata`, 443 → 9443), `rbac.yaml` (ClusterRole/Binding + leases
Role/Binding), `serviceaccount.yaml`, `certificate.yaml` (self-signed `Issuer`
`talos-os-metadata-selfsigned` + `Certificate` `talos-os-metadata-serving-cert` writing Secret
`talos-os-metadata-serving-cert`, dnsNames `talos-os-metadata.<ns>.svc` and
`talos-os-metadata.<ns>.svc.cluster.local`), `validatingwebhookconfiguration.yaml`
(`talos-os-metadata-webhook`, annotation
`cert-manager.io/inject-ca-from: <ns>/talos-os-metadata-serving-cert`). This is the repo's first
TLS machinery; the cainjector path works for `ValidatingWebhookConfiguration` (unlike
ExtensionConfig — F8), which is exactly why C2 carries no Runtime SDK registration burden.

Values → flags (kebab-case flags, camelCase values, repo convention):

| Value | Flag | Default |
| --- | --- | --- |
| `webhook.enabled` | `--enable-webhook` | `false` (opt-in — the default deployment is mirror-only, M1; setting `true` is the documented opt-in step and requires cert-manager) |
| `webhook.requireClassifiedMarker` | `--require-classified-marker` | `false` (document: enable when C1 is deployed) |
| `webhook.failurePolicy` | (manifest only) | `Fail` |
| `webhook.port` | `--webhook-port` | `9443` |
| `webhook.certDir` | `--webhook-cert-dir` | `/tmp/k8s-webhook-server/serving-certs` |
| `metricsBindAddress` | `--metrics-bind-address` | `:8080` |
| `healthProbeBindAddress` | `--health-probe-bind-address` | `:8081` |
| `leaderElect` | `--leader-elect` | `true` |
| `logLevel` / `logFormat` | `--log-level` / `--log-format` | `info` / `json` |

Leader election ID `talos-os-metadata.osmeta.tinkerbell.org`. The mirror controller is
leader-gated; the webhook server serves on every replica (relevant if replicas are ever raised for
webhook availability under `failurePolicy: Fail`).

## Implementation plan

Packages (single module, per repo convention):

- `cmd/talos-os-metadata/main.go` — flags, `internal/logging` slog factory, scheme
  (`clientgoscheme` + `tinkv1` + CAPT `api/v1beta2`), manager with webhook server, wiring.
- `internal/osmeta/mapping.go` — the shared pure core:

```go
// ParseDiskImageURL splits <factory>/image/<id>/<ver>/metal-<arch>.raw.<ext>.
func ParseDiskImageURL(u string) (schematicID, version, arch string, err error)

// DesiredOperatingSystem computes the mirror block from the status trio.
// Returns (nil, nil) when the trio is empty; an error when it is incoherent.
func DesiredOperatingSystem(st infrastructurev1.TinkerbellMachineStatus) (*tinkv1.MetadataInstanceOperatingSystem, error)

// Current reports whether hw already carries exactly want.
func Current(hw *tinkv1.Hardware, want *tinkv1.MetadataInstanceOperatingSystem) bool
```

- `internal/osmeta/reconciler.go`:

```go
type MirrorReconciler struct {
    client.Client
    Recorder record.EventRecorder
}

func (r *MirrorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
func (r *MirrorReconciler) SetupWithManager(mgr ctrl.Manager) error
```

  `SetupWithManager`: `For(&infrastructurev1.TinkerbellMachine{})` with a predicate passing
  create events and updates where the trio, `spec.hardwareName`, or `status.targetNamespace`
  changed; `Watches(&tinkv1.Hardware{}, handler.EnqueueRequestsFromMapFunc(hardwareToOwner))` where
  `hardwareToOwner` builds the request from the `v1alpha1.tinkerbell.org/ownerName|ownerNamespace`
  labels (no index needed — the labels carry the full key). The SSA write goes through
  `r.Patch(ctx, obj, client.Apply, client.FieldOwner("talos-os-metadata"), client.ForceOwnership)`
  on an `unstructured.Unstructured` carrying apiVersion/kind/name/namespace/resourceVersion and
  only the `spec.metadata.instance.operating_system` path.

- `internal/osmeta/webhook.go`:

```go
type WorkflowValidator struct {
    Reader                  client.Reader // manager cache
    RequireClassifiedMarker bool
    decoder                 admission.Decoder
}

func (v *WorkflowValidator) Handle(ctx context.Context, req admission.Request) admission.Response
```

  Registered at `/validate-tinkerbell-org-v1alpha1-workflow`. Deny responses use
  `admission.Denied(msg)`; row-3 admits use `admission.Allowed("").WithWarnings(...)`. No API
  writes anywhere in the handler (`sideEffects: None` is literally true).

- `internal/osmeta/diagnose.go` — `func talosVersionFor(ctx, c client.Reader, m *infrastructurev1.TinkerbellMachine) string`,
  the unstructured `Machine` → `TalosConfig` `spec.talosVersion` walk (port of CAPT
  `controller/machine/schematic.go:77-112`; validated against the same
  `^v\d+\.\d+\.\d+(-...)?$` shape). No `sigs.k8s.io/cluster-api` Go dependency is taken —
  unstructured reads only, mirroring CAPT's own rationale for not importing bootstrap-provider
  types.

New Go dependencies: `github.com/tinkerbell/cluster-api-provider-tinkerbell/api` (standalone module
— importable without the controller) — `tinkerbell/tinkerbell/api` is already present.

Milestones:

- **M1 — mirror controller.** `mapping.go` + `reconciler.go` + events/metrics; chart in its
  default mirror-only shape (`webhook.enabled=false`). Independently useful (live tootles
  metadata) and the default deployment. Exit: envtest green, mirror visible on a kind cluster
  with hand-crafted status.
- **M2 — webhook + TLS.** `webhook.go`, decision table rows 1-3 and 5, certificate/webhook chart
  templates, `failurePolicy` plumbing. Exit: envtest admission tests green; deny→mirror→admit
  convergence demonstrated against a real CAPT reconcile loop in a dev cluster.
- **M3 — classified-marker check + hardening.** Row 4 behind the flag, metrics
  (`osmeta_mirror_writes_total`, `osmeta_mirror_conflicts_total`,
  `osmeta_admission_decisions_total{verdict,reason}`, `osmeta_degraded{reason}`), docs, P7
  cross-repo recommendation filed against the terraform template.

Test strategy (repo style: table-driven with fakes; no Talos API involved, so no fake-Talos-API
harness is needed for this component):

- Unit: `ParseDiskImageURL` / `DesiredOperatingSystem` / `Current` tables (xz and zst inputs,
  pre-release versions, both arches, garbage URLs); `WorkflowValidator.Handle` against a fake
  client and hand-built `admission.Request`s covering all five table rows.
- envtest: CRD fixtures for `tinkerbell.org` and `infrastructure.cluster.x-k8s.io` pinned under
  `test/crds/` (copied from the dependency modules' `config/crd/bases`); scenarios: mirror on trio
  arrival, re-mirror after an external erase (simulated discovery stomp), refusal to write after
  release (owner labels removed), resourceVersion-conflict requeue, and the full webhook wiring
  with envtest's webhook install options.
- The deny/backoff interaction with real CAPT is integration-only (dev cluster), not CI.

## Non-goals

- Computing schematics or contacting the Image Factory in any way (F2 — CAPT's job; C2 has no
  Factory client and no network dependency beyond the API server).
- Clearing `operating_system`, instance state, or `userData` on hardware release (C4's contract; C2
  never clears).
- Writing C1's labels/annotations, `talos.tinkerbell.org/*` schematic inputs, or `spec.userData`.
- Acting on released or never-claimed Hardware, or on unlabeled (auto-enrollment) Workflows.
- Migrating the workflow template to `{{ .diskImageURL }}` — recorded as a cross-repo
  recommendation (P7), executed in tfc/cluster-bootstrap.
- Blocking or gating anything when schematic resolution never ran (fail-open by design).

## Open questions

- After the P7 template migration to `{{ .diskImageURL }}` is verified on hardware, should the
  documentation still recommend opting in to the webhook (then ecosystem-compat only — a cheap
  freshness guard for tootles consumers), or drop the recommendation entirely? Leaning: keep
  recommending the opt-in until a consumer inventory says otherwise.
- Should C2 also mirror `metadata.instance.state` (e.g. `active` after provisioning) for tootles
  fidelity? No component owns writing it today; C4 clears it on release. Deferred until a concrete
  consumer appears.
- If real-world post-denial convergence is slowed by CAPT's backoff cap after long C2 outages, is
  the rejected nudge-annotation (or a CAPT-side Hardware watch, a one-line cross-repo change) worth
  revisiting?
- Multi-cluster placement: when external-Tinkerbell mode is ever used, the mirror needs access to
  both the CAPI cluster (TinkerbellMachine) and the Tinkerbell cluster (Hardware/Workflow) — a
  two-client variant not designed here.
