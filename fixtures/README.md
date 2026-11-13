# Saved examples

All data is made up, the bearer tokens included: they are plain words that
open nothing.

## `cases/`: calls to `/webhook`

Every intake rule has a directory under `cases/`. The format of the call is
in [`contract/v1/push.md`](../contract/v1/push.md).

| File | What it is |
| --- | --- |
| `request.body` | The exact bytes posted to `/webhook`. |
| `authorization.txt` | Optional. The exact `Authorization` header values of the call, one per line; an empty file means the call has no such header. When the file is absent the call carries Plainforge's made-up token, `Bearer made-up-token-of-plainforge`. |
| `response.txt` | The exact answer: the status line, a blank line, the body. |
| `job.json` | The Job manifest Keelworks submits. Present only when a Job is started. |
| `run.json` | What `/runs/<run>` says once the Job has ended. Present only when a Job is started. |
| `job-timeout.txt` | Optional. The job timeout for this case; ten minutes when absent. |

`standin.json` is the script for the stand-in cluster. `outcomes` says how the
tests of each made-up commit end; a commit that is not in it has no outcome,
and its Job is reported lost. `callers` lists the made-up bearer tokens the
stand-in knows, with the user and the audiences it reports for each; a token
that is not in it is one the cluster does not vouch for.

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
case that starts a Job when no `job.json` is saved. After a deliberate change
of behaviour, regenerate the files under `cases/` with
`go test ./internal/controller -update` and read the diff before committing
it. The files under `tokenreview/` are written by hand.
