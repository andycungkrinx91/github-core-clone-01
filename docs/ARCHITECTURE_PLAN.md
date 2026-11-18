# Architecture & Implementation Plan: `github-core-clone-01`

> Status: Proposed plan awaiting user approval. No source code has been written.

## 1. Execution Model

- **Entrypoint (`main.ts`):** Minimal Node 22 launcher at the repository root that spawns the vendored statically linked `linux/amd64` Go binary (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`, `-ldflags="-s -w"`, verified `< 8 MiB` single file and `< 16 MiB` total repo size).
- **Noexec Safety:** Attempt direct execution from `/bot/`; if `EACCES`/`EPERM` occurs due to read-only/noexec mount, copy/stream to `/tmp/` once at startup and execute.
- **Lifecycle:** The Go server binds `:0` (ephemeral, never `8080`), writes `{"port": N}` to `$MAKEFUN_ENV_URL/register`, answers `GET /healthz` (`200 {"ok": true}`), and polls `GET $MAKEFUN_ENV_URL/done` until `{"done": true}`, then exits `0`.

## 2. Shared In-Memory Store & Determinism

- Single in-memory store protected by `sync.RWMutex` + immutable pre-serialized JSON response buffers on hot read objects (such as issues).
- Zero wall-clock timestamps (`time.Now()`) or unseeded randomness (`math/rand`) in any persisted or returned deterministic state; use `MAKEFUN_TRIAL_NOW` and sequential deterministic IDs (`repoId`, `issueId`, `prId`, `commentId`, opaque Base64 Relay global IDs).
- Token identity: Validate `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>` (`401` if missing/invalid). Repositories created under the token belong to `adaiversen` (`buildScenario().viewer.login`). Access to any repo under another owner returns `404` JSON.

## 3. Protocol Surfaces (120 Conformance Checks)

1. **Git Smart HTTP (`/{owner}/{repo}.git/...`, 30 checks):**
   - Pure-Go SHA-1 object store (blob/tree/commit/tag) + zlib + packfile v2 parser (`OBJ_OFS_DELTA` & `OBJ_REF_DELTA` resolution) and packfile writer.
   - `git-upload-pack`: Protocol v2 capability advertisement (`Git-Protocol: version=2`), `ls-refs` (`ref-prefix`, `symrefs`, `peel`), `fetch` (`want`, `have`, `done`, `shallow`, `deepen`, `side-band-64k` packfile framing), plus v0 fallback (`# service=git-upload-pack`).
   - `git-receive-pack`: Protocol v0 advertisement (`# service=git-receive-pack`, `report-status`, `side-band-64k`, `delete-refs`), unpack, per-ref fast-forward ancestry check (`ok <ref>` / `ng <ref> non-fast-forward`), branch/tag create/delete, mixed ref reports.
2. **REST v3 + Protocol (`/api/v3/...`, 55 checks):**
   - Global middleware: `application/json; charset=utf-8` always, non-empty `X-GitHub-Request-Id` on every reply, `400` on malformed JSON, `404` JSON on unknown routes.
   - `GET /api/v3/` (`X-OAuth-Scopes: repo, read:org`), `GET /api/v3/meta` (`installed_version`), `GET /api/v3/user` (`login: "adaiversen"`).
   - Repos (`/user/repos` with `Link` pagination, `GET`/`PATCH`/`DELETE /repos/{o}/{r}`), Git refs/branches/commits, Contents (`PUT`/`GET`/`DELETE /contents/{path}` with `409` stale SHA & directory listing).
   - Labels (`POST /labels`) & Issues (`POST`/`GET`/`PATCH` with `422` unknown label, open/closed filter, asc/desc sort, comments).
   - Pull Requests (`POST`/`GET`, `/files` tree diff, `PUT /merge` squash commit onto base branch + 3-way conflict check returning `405`).
3. **GraphQL v4 (`POST /api/graphql`, 20 checks):**
   - Single & batched JSON array operations, `401` unauthenticated, `200` with `errors` on unknown fields, `data.repository = null` on unknown repo.
   - `__type` introspection (`Issue`, `PullRequest`, `Repository`, `SearchType`), `viewer`, `node(id:)`, `repository` (`issues` cursor pagination + `CREATED_AT DESC` + `states: [OPEN]`, `pullRequests`, `pullRequest.files`), and mutations (`createIssue`, `closeIssue`, `createPullRequest`, `mergePullRequest`, `addComment`).
4. **MCP Streamable HTTP (`POST /mcp`, 15 checks):**
   - Support `2026-07-28` (`server/discover` + `_meta`) and `2025-11-25` (`initialize` + `notifications/initialized`).
   - Deterministic `tools/list` with `create_repo`, `get_repo`, `create_issue`, `list_issues`, `create_pull_request`, `merge_pull_request`, `tools/call` mapped to the shared store, concurrency-safe JSON-RPC IDs, `-32700` (HTTP 400) on invalid JSON, `-32601`/`-32602`/`isError: true`, and `401` without auth.

## 4. Performance Hot-Path Targets (200 Points)

- **`read` (2,000 sequential `GET /issues/{n}`):** Pre-serialized immutable JSON response bytes per issue + raw HTTP/1.1 keep-alive writer.
- **`write` (200 sequential `POST /issues`):** Minimal-allocation JSON parser/formatter, zero disk I/O.
- **`push` (200 sequential pushes):** Pooled `zlib.Reader`, single-pass pack parsing, O(1) commit-parent ancestry cache.
- **`clone` (50 concurrent shallow depth-1 clones):** Pre-built depth-1 packfile cache keyed by tip OID so concurrent clones serve cached pack bytes with zero redundant tree/blob traversal or re-compression.

## 5. Validation Gates

1. `sdk/run.ts --url ... --token ... --perf` -> target `120/120` checks (`feature: 600`, `agent: 200`) and benchmark timing measurement.
2. `node cli/makefun.mjs grade-local --kit . --bot <repo> --driver docker` (or `--driver process`) -> verify live + replay byte-identical `state.json`.
3. `node cli/makefun.mjs bundle --repo <repo> --dry-run` and `node cli/makefun.mjs check` -> verify transcript coverage, run record, license, README, and size limits before any push/submission.
