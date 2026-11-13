# Saved examples

Every intake rule has a directory under `cases/`. All data is made up.

| File | What it is |
| --- | --- |
| `request.body` | The exact bytes Plainforge posts to `/webhook`. |
| `response.txt` | The exact answer: the status line, a blank line, the body. |
| `job.json` | The Job manifest Keelworks submits. Present only when a Job is started. |
| `run.json` | What `/runs/<run>` says once the Job has ended. Present only when a Job is started. |
| `job-timeout.txt` | Optional. The job timeout for this case; ten minutes when absent. |

`standin.json` is the script for the stand-in cluster: how the tests of each
made-up commit end. A commit that is not in it has no outcome, and its Job is
reported lost.

`go test ./...` replays every case and fails on any difference, including a
case that starts a Job when no `job.json` is saved. After a deliberate change
of behaviour, regenerate with `go test ./internal/controller -update` and
read the diff before committing it.
