# Orchestrator ↔ AI Engine Contract

Internal HTTP contract between the Go orchestrator and the Python AI
service. Both live in this monorepo — treat changes here like any other
code change requiring review from whoever owns the other side, not a
versioned external API.

Last reviewed: 2026-09-19 — GitHub Copilot

---

## 1. Orchestrator → AI Engine

**Route:** `POST <AI_ENGINE_URL>` (the Python service registers this as `/`)
**Auth:** `HMAC-Signature-256` header — HMAC-SHA256 of the raw request
body, hex-encoded, signed with the shared `INTERNAL_SECRET`.
**Headers:**
| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `Job-Type` | Go currently permits `open`, `close`, `test_results`, `edit`, `sync`, `init` |
| `HMAC-Signature-256` | see above |

**Body as currently serialized by Go:** `AIEngineRequest`.
The Go type uses explicit lower-camel JSON tags; it is not serialized with
capitalized keys.

| Field | Type | Notes |
|---|---|---|
| `wfid` | int | mandatory workflow id |
| `pullRequest` | PullRequest | see shared type below |
| `changedFiles` | `[]FileDiff` | optional changed-file contents |
| `testResults` | TestResults | optional test-run result; see fields below |
| `error` | string | optional workflow error before container execution |

`TestResults` is serialized as `stdout`, `stderr`, `startTime`, `endTime`,
`errors`, `status`, `oomKilled`, and `exitCode`. `startTime` and `endTime`
are Go `time.Time` values serialized as RFC3339 timestamps.

**Response:** The Python handler returns `200 OK` with
`{"status":"accepted"}` after validating the HMAC and JSON. Invalid job
types return `400`, invalid signatures return `401`, and invalid JSON returns
`422`. The Go caller treats any non-`200` response as failed and surfaces it
via `ErrorObject`.

**Job-Type semantics:**
| Job-Type | Meaning | AI Engine behavior |
|---|---|---|
| `open` | PR opened/reopened | Python generates an initial test and sends a callback with `Done=false` |
| `edit` / `sync` | PR metadata / branch head changed | ack only |
| `logs` | test run finished | Python accepts this, but Go's configured request-type list does not currently permit it; when handled, `ExitCode==0` sends `Done=true`, otherwise it generates a test and sends `Done=false` |
| `close` | workflow closed | sends a callback with `Done=true` |
| `test_results` / `init` | permitted by Go configuration | not accepted by the Python handler (`400`) |
| `seed` | not currently supported | neither side currently lists this as an accepted job type |

---

## 2. AI Engine → Orchestrator (callback)

**Route:** `POST <ORCHESTRATOR_URL>/aiengine`

**Auth:** `HMAC-Signature-256` header, same scheme as above, signed with
the same `INTERNAL_SECRET` (called `ai_engine_secret` on the Python side —
confirm these resolve to the same value).

**Body intended by the Go type:** `AIEngineResponse` with lower-camel JSON
tags on all fields. The current Python callback client instead sends
capitalized top-level keys, so this is an implementation inconsistency; see
the note below.

| Field | Type | Notes |
|---|---|---|
| `wfid` | int | correlates back to the originating workflow |
| `pullRequest` | PullRequest | |
| `testCmd` | `[]string` | command used to run generated tests |
| `tests` | `[]FileDiff` | ignored if `done`; not a base64-encoded `[]byte` |
| `done` | bool | if true, no more test-authoring rounds |
| `summary` | string | required when `done` |

**Response:** `200 OK` is returned by the orchestrator after accepting and
queueing the callback. Other validation failures return a non-`200` response.
The current Python client logs a non-`200` response but does not retry.

**Delivery guarantee:** none — plain HTTP, no broker. The current Python
caller does not retry failed callbacks.

---

## 3. Shared type: `PullRequest`

| Field | JSON key | Notes |
|---|---|---|
| `RepoName` | `repoName` | repository name |
| `Owner` | `repoFullName` | repository owner/login despite the key name |
| `Number` | `number` | |
| `Action` | `action` | GitHub PR action string |
| `Branch` | `branch` | head ref |
| `Title` / `Body` | `title` / `body` | |
| `HeadSHA` / `BaseSHA` | `headsha` / `basesha` | |
| `Merged` | `merged` | |
| `CommentsURL` | `url` | |

---

## 4. Known implementation inconsistencies

This document records the current implementations; it does not resolve their
wire-format differences:

- Go sends `AIEngineRequest` with lower-camel keys (`wfid`, `pullRequest`,
	`changedFiles`, `testResults`, `error`), while the Python `JobRequest`
	expects capitalized keys and a flattened set of test-result fields, plus
	`RepoUrl`. As written, the two request models do not match.
- Go expects `AIEngineResponse` fields `wfid`, `pullRequest`, `testCmd`,
	`tests`, `done`, and `summary`, where `tests` is `[]FileDiff`. Python sends
	capitalized keys and encodes `Tests` as a base64 string, and also sends
	`TestName`, which Go does not define. The callback models therefore do not
	match.
- The contract's shared-secret statement assumes `INTERNAL_SECRET` is the
	same value on both sides. Python explicitly maps `INTERNAL_SECRET` to
	`ai_engine_secret`; deployment must still provide the same value to both
	services.

---