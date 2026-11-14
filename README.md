<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo-dark.svg">
    <img src="brand/logo-light.svg" alt="Keelworks" height="56">
  </picture>
</p>

<p align="center">
  <strong>Tests, builds and signs an image for every change.</strong><br>
  Part of <a href="https://clearedstack.com">ClearedStack</a>
</p>

---

> **Status: ready to deploy.** Keelworks takes authenticated push notifications from Plainforge, runs tests and image builds in ephemeral `ko` or rootless BuildKit Jobs, verifies `cosign` signatures in Lazarette, and commits pinned digests back to `app.yaml` in Plainforge with `[skip-ci]`. Both the real in-cluster launcher and deterministic `-standin` replay mode are built and tested, alongside hardened Kubernetes manifests in [`deploy/`](deploy). See [What works today](#what-works-today).

Keelworks is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Keelworks is meant to be understood and used without the rest.

## What works today

Keelworks is a single Go program (`module github.com/ClearedStack/keelworks`) with no dependencies outside the Go standard library.

**Built and tested**

- **Taking a push notification.** `POST /webhook` reads a JSON object with four string fields: `repository`, `ref`, `before` and `after`. Pushes to `main` or tags for the 16 buildable repositories in the ClearedStack naming registry (`clearedstack.com` for Conn, plus the 15 Go control-plane services and workers) are accepted; pushes to other branches are ignored. The format is written down in [`contract/v1/push.md`](contract/v1/push.md), and it is the only one accepted.
- **Taking it from Plainforge only.** Every call to `/webhook` must carry a bearer token of the ServiceAccount `plainforge` in the namespace `clearedstack`, issued for the audience `keelworks`. Keelworks checks the token with a Kubernetes `TokenReview` on every call, before it reads the body. No token is 401, a token of anyone else is 403, and a cluster that does not answer is 503. `/healthz` and `/runs/<run>` ask for no token.
- **No `[skip-ci]` rule on intake.** Keelworks does not receive commit messages. Whether a push is announced at all is decided by Plainforge, which suppresses notifications for commits carrying `[skip-ci]`.
- **Refusing instead of guessing.** Malformed JSON is refused with `E_PARSE`. Valid JSON that no rule covers (extra fields, deleted refs, refs that are neither branches nor tags, or repositories without a build rule such as `logbook`) is refused with `E_NEEDS_SUBTOOL`. Every error detail is a fixed phrase written in this repository; nothing from outside is repeated in it.
- **Running tests, building and signing in an ephemeral Job.** For each accepted push Keelworks renders a Kubernetes `batch/v1` Job in `clearedstack` (`ko` for Go repositories, rootless BuildKit for `clearedstack.com`), submits it over HTTPS to the Kubernetes API, waits for completion, reads the published `sha256` digest from `/dev/termination-log`, and deletes the Job. Both Keelworks and its spawned Jobs target `arm64` (excluding `cm3588-plus`) and run as non-root UID `65532` with a read-only root filesystem, all Linux capabilities dropped, no privilege escalation, and no mounted service account token in build pods.
- **Verifying `cosign` signatures in Lazarette.** Before touching `app.yaml`, Keelworks queries Lazarette (`HEAD /v2/<image>/manifests/<digest>` and OCI 1.1 `/v2/<image>/referrers/<digest>`, with `sha256-<hex>.sig` tag fallback), checks the signature blob SHA-256, and verifies the ECDSA P-256 signature against `cosign.pub` over the `cosign` SimpleSigning payload for that exact manifest digest. Missing, unsigned or mismatched images fail the run.
- **Pinning `app.yaml` in Plainforge.** For `main` builds (and tag builds whose commit matches `refs/heads/main`), Keelworks fetches `main` from Plainforge over Git smart HTTP (`git-upload-pack`), updates the `digest:` field in `app.yaml`, and pushes a 3-object packfile commit titled `pin <app> to <digest> [skip-ci]` (`git-receive-pack`) with compare-and-swap retry if `main` advances concurrently.
- **Relationship to Muster and Logbook.** Under Muster's shared contract (`contract/v1/CONTRACT.md`), only the ten workers in `cs-workers` plus `conn` and `shipkeeper` in `clearedstack` call Muster. Keelworks is a control-plane service in `clearedstack` (`dependsOn: ["plainforge", "lazarette"]`) and does not call Muster or Logbook directly; instead, Shipkeeper reconciles Keelworks' `app.yaml` updates from Plainforge and reports live digests to Muster (`POST /v1/health`), triggering Muster and Logbook replays.
- **Reporting run state.** `GET /runs/<run>` reports `running`, `passed` (with verified `image` and `digest`, plus `pinned: true` when `app.yaml` was updated), `failed`, `timed-out` (with `E_TIMEOUT`), or `interrupted` (preempted, lost, refused by the cluster, Lazarette/Plainforge unavailable, or controller shutdown). Duplicate deliveries of a running or terminal run start only one Job; interrupted runs retry when redelivered.
- **Saved examples and end-to-end tests.** [`fixtures/cases`](fixtures/cases) holds 36 intake and pipeline cases (exact request, HTTP response, rendered `batch/v1` Job manifest, final run report, and `app.yaml` commit record), and [`fixtures/tokenreview`](fixtures/tokenreview) holds 15 `TokenReview` responses. `cmd/keelworks` also tests the real-cluster launcher end to end over TLS against a mock Kubernetes API, an OCI registry with real ECDSA P-256 signatures, and real `git http-backend`.
- **Kubernetes manifests.** [`deploy/`](deploy), [`app.yaml`](app.yaml), and [`.ko.yaml`](.ko.yaml) provide the `PriorityClass`, `ServiceAccount`/`Role`/`ClusterRoleBinding`, `Service`, hardened `Deployment` (`16Mi` request / `32Mi` limit), and default-deny `NetworkPolicy` opening only Kubernetes API, Plainforge (`8080`), Lazarette (`5000`), and cluster DNS.

**What this component needs from others before live rollout**

- **Plainforge** (`ClearedStack/plainforge`): attach `Authorization: Bearer <token>` from a projected ServiceAccount token (`audience: keelworks`) on `POST /webhook` notifications, serve repositories at `<plainforge-url>/<repo>.git` (and handle packed refs/objects if `git gc` runs on bare repositories), and allow ingress on TCP `8080` from `keelworks` and `keelworks-build` pods in `clearedstack`.
- **Lazarette** (`ClearedStack/lazarette`): support storing and serving `cosign` signature artifacts via OCI 1.1 `/v2/<name>/referrers/<digest>` (or `sha256-<hex>.sig` tags), which Lazarette's first milestone currently refuses with `E_NEEDS_SUBTOOL`, and allow ingress on TCP `5000` from `keelworks` and `keelworks-build` pods in `clearedstack`.
- **Cluster bootstrap and builder images**: generate Secret `keelworks-cosign` (`cosign.key` and `cosign.pub`) in-cluster in `clearedstack` (no keys are stored in Git), publish the pinned `static`, `keelworks`, `keelworks-ko` and `keelworks-buildkit` images into Lazarette (whose entrypoints read `KEELWORKS_*`, clone `$KEELWORKS_COMMIT`, run golden tests, build/push and `cosign`-sign `$KEELWORKS_IMAGE`, and write `sha256:<hex>` to `/dev/termination-log`), and replace the `sha256:0000...` bootstrap placeholder digests in `.ko.yaml`, `app.yaml` and `deploy/deployment.yaml`.
- **Shipkeeper** (`ClearedStack/shipkeeper`): reconcile `app.yaml` digest commits from Plainforge and report active digests to Muster via `POST /v1/health`.

### Run it

```sh
go vet ./...
go test -race ./...

go run ./cmd/keelworks -standin fixtures/standin.json \
  -ko-image lazarette.clearedstack.svc.cluster.local:5000/keelworks-ko@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
  -buildkit-image lazarette.clearedstack.svc.cluster.local:5000/keelworks-buildkit@sha256:2222222222222222222222222222222222222222222222222222222222222222 \
  -priority-class keelworks-build-low \
  -job-timeout 10m -token-review-timeout 5s

curl -X POST -H 'Authorization: Bearer made-up-token-of-plainforge' \
  --data-binary @fixtures/cases/tests-pass/request.body http://localhost:8080/webhook
# {"status":"accepted","run":"muster-674f2946f186"}
curl http://localhost:8080/runs/muster-674f2946f186
# {"run":"muster-674f2946f186",...,"state":"passed","image":"lazarette.clearedstack.svc.cluster.local:5000/muster","digest":"sha256:...","pinned":true}

curl -X POST --data-binary @fixtures/cases/tests-pass/request.body http://localhost:8080/webhook
# {"status":"refused","detail":"no bearer token"}
```

`made-up-token-of-plainforge` is a made-up token from `fixtures/standin.json`. It is not a credential for anything. Without `-standin`, `keelworks` connects to the in-cluster Kubernetes API, Lazarette, and Plainforge using mounted ServiceAccount credentials and `/var/run/secrets/keelworks/cosign/cosign.pub` (see [`deploy/deployment.yaml`](deploy/deployment.yaml)).

| Path | Purpose |
| --- | --- |
| `cmd/keelworks` | The program: flags, in-cluster and `-standin` wiring, HTTP server, shutdown. |
| `internal/caller` | Decides whether a call comes from Plainforge, and the `TokenReview` client. |
| `internal/push` | Reads a notification, resolves the build target, and decides: build, ignore or refuse. |
| `internal/registry` | Maps the 16 buildable ClearedStack repositories to image slugs and builders (`ko` or `buildkit`). |
| `internal/buildjob` | Renders ephemeral `batch/v1` Jobs, runs the build/verify/pin pipeline, and talks to the Kubernetes API. |
| `internal/lazarette` | Verifies pushed OCI manifests and ECDSA P-256 `cosign` signatures in Lazarette. |
| `internal/plainforge` | Updates `app.yaml` in Plainforge with `[skip-ci]` commits over Git smart HTTP. |
| `internal/controller` | The HTTP paths and the bounded table of runs. |
| `internal/standin` | Deterministic in-memory stand-ins for Kubernetes, `TokenReview`, Lazarette and Plainforge. |
| `internal/fault` | The three public error codes (`E_PARSE`, `E_TIMEOUT`, `E_NEEDS_SUBTOOL`). |
| `contract/v1` | The push notification format, for whoever builds the sending side. |
| `fixtures` | Saved intake, `TokenReview`, Job, run report, and `app.yaml` pin examples. |
| `deploy` | Kubernetes `PriorityClass`, RBAC, `Service`, hardened `Deployment` and default-deny `NetworkPolicy`. |

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Keelworks does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`app.yaml`](app.yaml) | Shipkeeper release manifest for Keelworks in `clearedstack`. |
| [`.ko.yaml`](.ko.yaml) | `ko` base image configuration pinned by digest in Lazarette. |
| [`contract/v1/push.md`](contract/v1/push.md) | The push notification Keelworks accepts from Plainforge. |
| [`brand/`](brand) | The Keelworks logo and mark, for light and dark backgrounds. |

## At a glance

| | |
| --- | --- |
| **Role** | Control-plane service |
| **Formerly** | `cs-runner`, then Slipway |
| **Namespace** | `clearedstack` |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/keelworks` |
| **Memory budget** | 32 MiB |
| **Depends on** | [Plainforge](https://github.com/ClearedStack/plainforge), [Lazarette](https://github.com/ClearedStack/lazarette) |
| **Depended on by** | Nothing else in the platform |

### Boundaries

- Receives push notifications from Plainforge
- Pushes signed images to Lazarette
- Writes the new image digest back to Plainforge

## Names

`keelworks` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
