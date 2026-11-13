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

> **Status: design.** The first milestone is built: Keelworks takes a push notification and runs that repository's tests in a temporary Job, reporting pass or fail. It runs only against in-memory stand-ins. It builds, signs and pushes nothing yet. See [What works today](#what-works-today).

Keelworks is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Keelworks is meant to be understood and used without the rest.

## What works today

Keelworks is a single Go program with no dependencies outside the standard library.

**Built and tested**

- **Taking a push notification.** `POST /webhook` reads a JSON object with exactly four string fields: `repository`, `ref`, `commit` and `message`. A push to `main` or a push of a tag is tested. A commit whose message contains `[skip-ci]` is ignored, and so is a push to any other branch.
- **Refusing instead of guessing.** A broken body is refused with `E_PARSE`. A valid body that no rule covers (an extra field, a deleted ref, a ref that is neither a branch nor a tag) is refused with `E_NEEDS_SUBTOOL`. Every error detail is a fixed phrase written in this repository; nothing from outside is repeated in it.
- **Running the tests in a temporary Job.** For each accepted push Keelworks renders a Kubernetes `batch/v1` Job in the `clearedstack` namespace, submits it, waits for it and deletes it. The Job runs once, as a non-root user, with every Linux capability dropped, no privilege escalation, no volumes and no service account token.
- **Reporting pass or fail.** `GET /runs/<run>` says `running`, `passed`, `failed`, `timed-out` (with `E_TIMEOUT`) or `interrupted` (preempted, lost, refused by the cluster, or Keelworks shut down). A notification delivered twice starts one Job. An interrupted run is started again when its notification is delivered again.
- **Saved examples.** [`fixtures/`](fixtures) holds 23 cases: the exact request, the exact answer, and for accepted pushes the exact Job manifest and final report. The tests replay all of them.

**Stand-ins, not the real thing**

Plainforge and the Kubernetes cluster do not exist here, so each sits behind a small interface with an in-memory stand-in in [`internal/standin`](internal/standin). The stand-in cluster runs no tests: it plays back, from a script, how the tests of each made-up commit end, and it refuses any Job that breaks the security rules of `SPEC.md`. **No launcher for a real cluster is built.** Without `-standin` the program refuses to start.

**Not yet done**

- A launcher that talks to a real Kubernetes API, and the test image the Job runs.
- Building images with `ko` or rootless BuildKit.
- Signing with `cosign` and pushing to Lazarette.
- Writing the digest back to `app.yaml` in Plainforge with a `[skip-ci]` commit.
- Deployment manifests: the controller's own pod, its `NetworkPolicy`, the PriorityClass.
- Building the Keelworks image itself with `ko`. There is no Dockerfile and there will not be one.
- Proof of the 32 MiB budget in a real pod. Run by hand as below, the stand-in build peaked at about 12 MiB of resident memory after 3,000 notifications; that is one measurement on one machine, not a test.

### Run it

```sh
go test ./...

go run ./cmd/keelworks -standin fixtures/standin.json \
  -test-image made-up-test-image -priority-class made-up-low-priority -job-timeout 10m

curl -X POST --data-binary @fixtures/cases/tests-pass/request.body http://localhost:8080/webhook
# {"status":"accepted","run":"muster-674f2946f186"}
curl http://localhost:8080/runs/muster-674f2946f186
# {"run":"muster-674f2946f186",...,"state":"passed"}
```

`-test-image`, `-priority-class` and `-job-timeout` have no defaults, because `SPEC.md` names no values for them.

| Path | Purpose |
| --- | --- |
| `cmd/keelworks` | The program: flags, HTTP server, shutdown. |
| `internal/push` | Reads a notification and decides: test, ignore or refuse. |
| `internal/testjob` | Renders the Job and runs it on a cluster behind an interface. |
| `internal/controller` | The HTTP paths and the table of runs. |
| `internal/standin` | In-memory stand-ins for the cluster and for Plainforge. |
| `internal/fault` | The three public error codes. |
| `fixtures` | The saved examples. |

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Keelworks does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
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
