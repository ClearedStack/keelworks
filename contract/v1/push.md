# Push notification, version 1

How Plainforge tells Keelworks that a ref moved. This is the only format Keelworks accepts. The saved examples under [`fixtures/cases`](../../fixtures/cases) are the exact bytes and the exact answers, and the tests replay them.

## The call

| | |
| --- | --- |
| **From** | Plainforge, inside the cluster |
| **Method and path** | `POST /webhook` |
| **Header** | `Authorization: Bearer <token>`, exactly once |
| **Body** | One JSON object, 65,536 bytes at most |

## The token

Keelworks takes a push from Plainforge and from nobody else.

- The token is a token of the ServiceAccount `plainforge` in the namespace `clearedstack`.
- It must be issued for the audience `keelworks`. A token issued for the cluster's own API is refused. Plainforge gets the right one from a projected ServiceAccount token volume with `audience: keelworks`.
- Keelworks hands the token to the cluster in a `TokenReview` (`authentication.k8s.io/v1`) that asks about the audience `keelworks`. The call is admitted only when the cluster answers that the token is authenticated, confirms the audience `keelworks`, and names the user `system:serviceaccount:clearedstack:plainforge`.
- Every call is checked again. Keelworks remembers no answer.
- The header value is `Bearer`, one space, then the token: letters, digits and `-._~+/`, then any number of `=`, 4,096 bytes at most. Anything else counts as no token, and the cluster is not asked.

The token is checked before the body is read. A caller that is not Plainforge learns nothing about the body it sent.

## The body

Exactly four fields, all strings. The order of the fields does not matter.

| Field | Shape | Meaning |
| --- | --- | --- |
| `repository` | A repository name: starts with a lowercase letter, ends with a lowercase letter or digit, and contains lowercase letters, digits, hyphens and single dots between letters or digits. 32 characters at most. | The repository that was pushed to. |
| `ref` | `refs/<kind>/<name>` in printable ASCII without spaces, 200 characters at most. | The full name of the ref that moved. |
| `before` | 40 lowercase hexadecimal characters. | The commit the ref pointed at before the push. All zeros for a ref that did not exist. |
| `after` | 40 lowercase hexadecimal characters. | The commit the ref points at now. This is the commit Keelworks builds. All zeros for a ref that was deleted. |

```json
{"repository":"muster","ref":"refs/heads/main","before":"abababababababababababababababababababab","after":"1111111111111111111111111111111111111111"}
```

`before` is checked for its shape and is not used for anything else today. The run is named from the component's slug (`conn` for `clearedstack.com`, or `repository` otherwise) and a hash of `repository`, `ref` and `after`.

### What is not in the body

There is no commit message, and Keelworks applies no `[skip-ci]` rule. Whether a push is announced at all is decided by Plainforge before it sends. A push that must not be built is a push Plainforge does not announce.

## What Keelworks does with it

The rules are applied in this order, and the first one that matches decides.

1. **Who is calling.** See the answers with status 401, 403 and 503 below.
2. **Broken data: `E_PARSE`.** The body is over the limit, is not exactly one JSON object, or one of the four fields is missing, is not a string, or does not have its shape. Fields are checked in alphabetical order: all four for missing or not a string, then all four for shape.
3. **Valid and unfamiliar: `E_NEEDS_SUBTOOL`.** In this order: the object has a fifth field; `after` is all zeros (a deleted ref); `ref` is neither `refs/heads/...` nor `refs/tags/...`; `repository` has no builder rule in Keelworks.
4. **Ignored.** A push to a branch other than `main`.
5. **Built.** A push to `main`, or a push of a tag. Keelworks starts one build Job for the commit in `after`.

## The answers

Every answer is one line of JSON. `detail` is always one of the fixed phrases below; nothing the caller sent and nothing the cluster said is repeated in it.

| Status | Body | When |
| --- | --- | --- |
| 202 | `{"status":"accepted","run":"<run>"}` | A build Job was started. `GET /runs/<run>` reports how it ends. |
| 200 | `{"status":"known","run":"<run>"}` | The same `repository`, `ref` and `after` were delivered before. Nothing new is started, unless that run was interrupted; then it is started again and the answer is 202. |
| 200 | `{"status":"ignored","reason":"branch-not-main"}` | A push to a branch other than `main`. |
| 400 | `{"status":"refused","code":"E_PARSE","detail":"..."}` | Broken data. |
| 422 | `{"status":"refused","code":"E_NEEDS_SUBTOOL","detail":"..."}` | Valid and unfamiliar. |
| 401 | `{"status":"refused","detail":"no bearer token"}` | No `Authorization` header, more than one, or one that is not a well-formed bearer token. Sent with `WWW-Authenticate: Bearer`. |
| 401 | `{"status":"refused","detail":"bearer token is not valid"}` | The cluster does not vouch for the token, or not for the audience `keelworks`. Sent with `WWW-Authenticate: Bearer`. |
| 403 | `{"status":"refused","detail":"caller is not Plainforge"}` | The token is valid and belongs to someone else. |
| 503 | `{"status":"refused","code":"E_TIMEOUT","detail":"token check took too long"}` | The cluster did not answer the `TokenReview` in time. |
| 503 | `{"status":"refused","detail":"token could not be checked"}` | The cluster gave no usable answer to the `TokenReview`. |
| 503 | `{"status":"refused","detail":"too many runs in progress"}` | Keelworks is at its limit of runs, or is shutting down. Sent with `Retry-After: 60`. |
| 405 | `{"status":"refused","detail":"method not allowed"}` | Any method other than `POST`. |

The details of `E_PARSE` are `body is too large`, `body is not valid JSON`, `body is not a JSON object`, `body could not be read`, and for each field `field <name> is missing`, `field <name> is not a string` and `field <name> is malformed`.

The details of `E_NEEDS_SUBTOOL` are `notification has a field no rule covers`, `no rule for a deleted ref`, `no rule for this kind of ref` and `no rule for this repository`.

Of the refusals, only a 503 is worth sending the same call again for. Sending a notification twice is safe: it names the same run.

## Changing this

A change to the fields, to the token rule or to an answer is a new version in a new directory beside this one. Version 1 does not change in place.
