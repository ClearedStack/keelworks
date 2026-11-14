# Saved examples

All data is made up, the bearer tokens and image digests included: they are
synthetic values that open nothing.

## `cases/`: calls to `/webhook`

Every intake, builder, verification and manifest-pinning rule has a directory
under `cases/`. The format of the call is in [`contract/v1/push.md`](../contract/v1/push.md).

| File | What it is |
| --- | --- |
| `request.body` | The exact bytes posted to `/webhook`. |
| `authorization.txt` | Optional. The exact `Authorization` header values of the call, one per line; an empty file means the call has no such header. When the file is absent the call carries Plainforge's made-up token, `Bearer made-up-token-of-plainforge`. |
| `response.txt` | The exact answer: the status line, a blank line, the body. |
| `job.json` | The `batch/v1` Job manifest Keelworks submits (`ko` or rootless `buildkit`). Present only when a Job is started. |
| `run.json` | What `/runs/<run>` says once the run has ended. Present only when a Job is started. |
| `pin.json` | The `[skip-ci]` commit Keelworks wrote to `app.yaml` in Plainforge. Present only when the Job passed and Lazarette held a verified `cosign` signature for the built digest. |
| `job-timeout.txt` | Optional. The job timeout for this case; ten minutes when absent. |

`standin.json` is the script for the stand-in cluster, Lazarette and Plainforge.
`outcomes` says how each made-up commit ends (`pass`, `fail`, `sign-fail`,
`unsigned`, `stall` or `preempt`); a commit that is not in it has no outcome,
and its Job is reported lost. `manifests` overrides initial `app.yaml` files in
Plainforge. `callers` lists the made-up bearer tokens the stand-in knows.

## `tokenreview/`: what the cluster says about a token

`request.json` is the exact `TokenReview` Keelworks sends for Plainforge's
made-up token. Each directory under `tokenreview/cases/` is one answer a
cluster can give:

| File | What it is |
| --- | --- |
| `status.txt` | The HTTP status of the cluster's answer. |
| `answer.json` | The exact body of the cluster's answer. |
| `verdict.txt` | What Keelworks must conclude about the caller. |

## Replaying

`go test ./...` replays every case and fails on any difference, including a
case that starts a Job when no `job.json` is saved or writes a Plainforge
commit when no `pin.json` is saved. After a deliberate change of behaviour,
regenerate the files under `cases/` with `go test ./internal/controller -update`
and read the diff before committing it. The files under `tokenreview/` are
written by hand.
