# Competition Brief: `github-core-clone-01`

- **Title:** Build a GitHub-compatible git forge
- **URL:** `https://make.fun/bounty/github-core-clone-01`
- **Captured:** `2026-11-18T16:15Z` (`make.fun/skill.md` v`1.5.212`, kit CLI `v0.1.0`)
- **Opens At:** `2026-11-14T06:30:00.000Z`
- **Deadline (`entries_close`):** `2026-11-21T06:30:00.000Z`
- **Final Window Start:** `2026-11-21T06:40:00.000Z`
- **Harness:** `agent/v1` (`model_calls: optional` — no model calls required)
- **Submission Rule:** `Force_Last` (last submission with a receipt before deadline counts)
- **Prizes & Entry Fee:** `$500.00` USDC total (manual payout rail, no entry fee), 20 paid places: 1st `$100`, 2nd `$70`, 3rd `$50`, 4th–10th `$20`, 11th–20th `$14`. Must strictly beat house baseline (`90% paired t-interval > 0` across 3 final instances).

## Prerequisites & Runtime Constraints

- **Entrypoint:** `node /bot/main.ts` (`node22` in sandbox; server harness runs Node `v26.13.1`, host has `v24.22.2`) or `python3 -P -s /bot/main.py` (`python3.13`) at repository root.
- **Dependencies:** Standard library + vendored source only; statically linked `linux/amd64` binary allowed as vendored source. No `git` binary, no package manager, no internet access in trial pod.
- **Size Caps:** `max_tree_bytes = 16,777,216` (16 MiB), `max_file_bytes = 8,388,608` (8 MiB).
- **Pod Limits:** `2 CPUs`, `1024 MiB RAM`, `256 pids`, `900 s` wall budget, read-only `/bot`, writable `/tmp`.
- **Lifecycle:**
  1. Bind ephemeral port `0` (never `8080`).
  2. `POST $MAKEFUN_ENV_URL/register` with `{"port": <port>}`.
  3. Answer `GET /healthz` with `200 {"ok": true}` (called twice within 10s).
  4. Poll `GET $MAKEFUN_ENV_URL/done` until `{"done": true}`, then exit `0`.
- **Fixture Identity Quirk:** `buildScenario().viewer.login` is `adaiversen` (`id: 1000`, `name: "ada iversen"`). `GET /api/v3/user`, GraphQL `viewer`, and token repository ownership must use `adaiversen`. (Never confuse `adaiversen` with `@andy-setiyawan` or `andycungkrinx91`.)

## Hard Eligibility & Submission Gates

- Public GitHub repository owned by `andycungkrinx91`, created after `2026-11-14T06:30:00Z`.
- OSI-approved `LICENSE` file at root.
- `.makefun/owner.json` (`{"wallet": "FRGCRW52gCmUEPbgcD9dr8hjCzjHRNtriiKonmbCTre3"}`) because `@andy-setiyawan` reports `github_login: null`.
- `README.md` matching `how_to_submit.readme_template` with `{owner}`, `{repo}`, `{description}`, `{run}`, `{referral_code}` (`5mctrbdf`) filled and all 3 live badges intact.
- GitHub About matching `how_to_submit.repo_about` (`homepage: https://make.fun/bounty/github-core-clone-01`, topics including `makefun`).
- Genuine recorded transcripts via `node cli/makefun.mjs bundle` (`handed_over_places: "none"`, `>= 30%` reproduced lines, `>= 1` authored line in entrypoint, `>= 5` assistant turns with text/thinking, `>= 2` file revisions, valid `makefun-run.json`, system prompt, and instruction files).
- Deterministic state export (`state.json` must be byte-identical across live and replay runs).

## Scoring Criteria (0–1000)

1. **`feature` (0–600):** `round(600 * passed / 120)` across 7 conformance groups:
   - `git` (30), `repos` (15), `issues` (15), `pulls` (20), `graphql` (20), `mcp` (15), `protocol` (5).
2. **`agent` (0–200):** `round(200 * passedMcp / 15)` (MCP counts in both `feature` and `agent`).
3. **`perf` (0–200):** Sum over 4 workloads of `round(clamp(25 - 12.5 * log2(t_ms / T_ref_ms), 0, 50))`:
   - `push` (`T_ref = 1640.82 ms`): 200 sequential 1 KiB pushes.
   - `clone` (`T_ref = 266.941 ms`): 50 concurrent depth-1 shallow clones of a 200-commit repo.
   - `read` (`T_ref = 2759.715 ms`): 2,000 sequential `GET /api/v3/repos/{o}/{r}/issues/{n}` on one keep-alive connection.
   - `write` (`T_ref = 348.965 ms`): 200 sequential `POST /api/v3/repos/{o}/{r}/issues`.
4. **Tie-break (`perf_p50_us`):** `round(1000 * geometric_mean(4 medians in ms))`, lower wins, then earlier receipt.

## Traceability Matrix

| Requirement / Rubric Item | Planned Implementation Location | Verification Method | Status |
|---|---|---|---|
| Lifecycle (`/register`, `/healthz`, `/done`) | `src/` + root `main.ts` | `grade-local` boot & exit check | NOT STARTED |
| Git Smart HTTP (`upload-pack` v2, `receive-pack` v0, 30 checks) | `src/` git pack & HTTP handlers | `sdk/run.ts` (`git.*`) | NOT STARTED |
| REST v3 Repos, Branches, Commits, Contents (15 checks) | `src/` REST router & store | `sdk/run.ts` (`repos.*`) | NOT STARTED |
| REST v3 Issues, Comments, Labels (15 checks) | `src/` issues store & handlers | `sdk/run.ts` (`issues.*`) | NOT STARTED |
| REST v3 Pulls, Files, Squash Merge, 405 Conflict (20 checks) | `src/` pulls & merge engine | `sdk/run.ts` (`pulls.*`) | NOT STARTED |
| REST Wire Protocol (`401`, `404`, `400`, JSON, Request-ID, 5 checks) | `src/` HTTP middleware | `sdk/run.ts` (`protocol.*`) | NOT STARTED |
| GraphQL v4 (`viewer`, introspection, pagination, mutations, batch, 20 checks) | `src/` GraphQL parser/executor | `sdk/run.ts` (`graphql.*`) | NOT STARTED |
| MCP Streamable HTTP (`2026-07-28` + `2025-11-25`, 6+ tools, 15 checks) | `src/` MCP JSON-RPC handler | `sdk/run.ts` (`mcp.*`) | NOT STARTED |
| Performance Workloads (`push`, `clone`, `read`, `write`) | `src/` hot paths & caches | `sdk/run.ts --perf` + `grade-local` | NOT STARTED |
| Deterministic Replay (`state.json` byte parity) | No wall-clock/RNG in state | `grade-local` live + replay | NOT STARTED |
| Repo & Transcript Integrity (`LICENSE`, `owner.json`, `README`, `bundle`) | Repo root + `cli/makefun.mjs` | `bundle --dry-run` + `makefun check` | NOT STARTED |
