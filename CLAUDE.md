# Runtime Hook Glue - Talos + Tinkerbell

Deeply analyze the terraform configuration from /home/appkins/src/tfc/cluster-bootstrap to understand what the current process of configuring a Talos cluster looks like with Tinkerbell.

Your goal is to bridge all the gaps - like:

Talos:

- extension selection
- machine config overrides (platform - ie Nvidia GPU found)
- Talos image factory schematic ID resolution (based on extensions, config, etc)
- Full raw image and installer URL resolution
- `talosctl` like reconciliation patterns like:
  - leaving etcd and properly removing node on machine teardown
  - running the upgrade container when changing versions of kubernetes
  - applying changed node config via the talos API

All of these tasks need to be cleanly implemented within the Tinkerbell CAPI infra provider's cluster lifecycle.

This should most likely be implemented via runtime extenion following <https://cluster-api.sigs.k8s.io/tasks/experimental-features/runtime-sdk/implement-lifecycle-hooks.html>

Follow the [guidance](https://cluster-api.sigs.k8s.io/tasks/experimental-features/runtime-sdk/implement-extensions#guidelines):
> Each Runtime Extension should accomplish its task without depending on other Runtime Extensions. Introducing dependencies across Runtime Extensions makes the system fragile, and it is probably a consequence of poor “Separation of Concerns” between extensions.

## Blocking Hooks

A good blocking hook example would be updating Tinkerbell hardware to include the proper Talos image data in `os` before templating or workflows run - so the template can properly resolve the image to install.

## Destructive hooks

Tinkerbell, when deleting a node, seems to *only?* power it off? This is obviously insufficient with Talos - especially with control plane nodes that need to be removed from the ETCD members to not break the other nodes.

## Runtime Extension - Type Selection

If altering data on a managed CRD, consider using [Mutation Hook Runtime Extensions](https://cluster-api.sigs.k8s.io/tasks/experimental-features/runtime-sdk/implement-topology-mutation-hook.html#implementing-topology-mutation-hook-runtime-extensions).

To retrieve the Talos version while updating Tinkerbell hardware, you may need to consider using [External variables](https://cluster-api.sigs.k8s.io/tasks/experimental-features/runtime-sdk/implement-topology-mutation-hook.html#external-variable-definitions).

---

## Layout

The repo should be a collection of runtime extensions, all of which can be deployed.

The repo must fill *all* gaps not captured by Tinkerbell to properly manage Talos nodes.

## Naming

Determine proper naming strategies per extension being implemented. Keep standards consistent, following guidance from kubernetes-sigs and then update this section with the standard.
