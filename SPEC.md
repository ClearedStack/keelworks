# Keelworks specification

> No-Docker build controller in the clearedstack namespace that spawns ephemeral ko and rootless BuildKit Jobs on push, runs golden tests, signs digests with cosign, and updates app.yaml.

**Status: design.** This is a specification. It describes how Keelworks is meant to work, not something that exists yet.

The same text is published at [clearedstack.com/platform/keelworks/](https://clearedstack.com/platform/keelworks/). To change the specification, change it here and say why.

## Overview & Role

Keelworks (`v0.1.0-draft`, owned by the Keelworks repository) is a lightweight MIT Go control-plane service running in the `clearedstack` namespace. It replaces traditional Docker-based CI runners with an event-driven, daemonless build controller.

When Plainforge receives a commit on `main` or a new tag, Keelworks spawns an ephemeral Kubernetes `Job` pod to run the repository's deterministic golden fixture tests, compile an OCI image using `ko` (for Go services and workers) or rootless BuildKit (for web apps like Conn), sign the resulting digest with `cosign`, push it to Lazarette, and commit the new immutable digest back to `app.yaml` in Plainforge.

## Runtime & Resource Budget

| Property | Value |
| --- | --- |
| **Service / Repo** | Keelworks (`keelworks`) |
| **Namespace** | `clearedstack` (`control-plane`) |
| **Version** | `v0.1.0-draft` |
| **RAM Budget (Requested)** | `32 MiB` (controller pod; build `Job` pods run ephemerally) |
| **Priority Class** | Standard / Low for ephemeral build `Job` pods (preempted before control-plane services under memory pressure) |
| **Builders** | `ko` (Go services & workers) and rootless BuildKit (Conn) |
| **Sub-Tools** | None (`[]`) |

## Interfaces & Data Contracts

Keelworks coordinates four internal control-plane interfaces:

1. **Webhook Intake (Plainforge -> Keelworks):** Receives cluster-private push and tag webhooks from Plainforge, in the format of [`contract/v1/push.md`](contract/v1/push.md). Each webhook must carry a bearer token of Plainforge's ServiceAccount, which Keelworks checks with a Kubernetes `TokenReview`. Plainforge decides whether a commit containing `[skip-ci]` in its message is announced at all; Keelworks does not receive commit messages.
2. **Ephemeral Job Orchestration (Kubernetes API):** Spawns short-lived `Job` pods in `clearedstack` with the appropriate builder:
   - `ko`: Compiles Go binaries (Plainforge, Lazarette, Keelworks, Shipkeeper, Muster, and Trawl through Bellbuoy) directly into minimal OCI images with zero Dockerfile or Docker daemon.
   - **Rootless BuildKit:** Builds multi-layer container images for Next.js web workloads (Conn) inside an unprivileged, rootless pod.
3. **Test, Sign & Push (`Job` -> Lazarette):** Executes golden fixture tests before building. Passing builds are signed with `cosign` and pushed by `sha256` digest to Lazarette.
4. **Manifest Pinning (Keelworks -> Plainforge):** Updates the pinned image digest in `app.yaml` inside Plainforge with a `[skip-ci]` commit so Shipkeeper can reconcile the new release.

## Sub-Tools & Failure Modes

Keelworks is a control-plane build controller and does not host `tools/<case>/` deterministic sub-tools (`subTools: []`).

| Failure Mode | Trigger | System Behavior & Recovery |
| --- | --- | --- |
| **Golden test failure** | A commit breaks a deterministic fixture or unit test | The build `Job` aborts before packaging or pushing; broken images never enter Lazarette. |
| **Build or push timeout (`E_TIMEOUT`)** | Slow dependency fetch or registry stall | The `Job` terminates cleanly and reports `E_TIMEOUT`; no partial digest is written to `app.yaml`. |
| **`cosign` signing failure** | Missing or invalid signing key material | Image push is blocked or left unsigned; Shipkeeper refuses to roll out unsigned digests. |
| **Memory preemption** | Node memory tightens during a build `Job` | Low PriorityClass allows `k3s` to preempt ephemeral build `Job`s first while protecting Logbook, Muster, and Shipkeeper; deterministic builds can be safely retried. |
| **Recursive CI loop** | Keelworks committing `app.yaml` back to Plainforge | Prevented deterministically by tagging `app.yaml` commits with `[skip-ci]`. |

## Security & Clean-Room Boundaries

- **Zero Docker daemon:** No Docker socket (`/var/run/docker.sock`), privileged containers, or root-level build daemons exist anywhere on the host or inside build `Job` pods.
- **Rootless & unprivileged:** Both the Keelworks controller and its spawned `ko` / rootless BuildKit `Job`s run as non-root users with dropped Linux capabilities and no privilege escalation.
- **Cluster-private only:** Keelworks has no public Ingress and accepts webhooks exclusively from Plainforge under default-deny `NetworkPolicy` rules. A webhook without a valid token of Plainforge's ServiceAccount is refused.
- **Supply-chain integrity:** Every published image is tested against golden fixtures and cryptographically signed with `cosign` before `app.yaml` is updated.

## Related pages

- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
- [The pipeline and its ten workers](https://clearedstack.com/platform/pipeline/)
