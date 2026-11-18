# Status: `github-core-clone-01`

- **Last Updated:** `2026-11-18T16:56:00Z`
- **Phase:** `Repository & Harness Ready (Awaiting Implementation Approval)`
- **Bounty:** `github-core-clone-01` — Build a GitHub-compatible git forge (`https://make.fun/bounty/github-core-clone-01`)
- **Deadline:** `2026-11-21T06:30:00.000Z`
- **Make.fun Account:** `@andy-setiyawan` (`01a1ef4f-2880-7002-bf83-a9df51288832`, `github_login: null`)
- **GitHub Account:** `andycungkrinx91`

## Verified Environment & Connection Snapshot

- **Container:** `portofolio-git-1` (`x86_64`, `git 2.34.1`, SSH authenticated as `andycungkrinx91`), workspace `/data/makefun/github-core-clone-01/` (independent local Git repo on `main`, no remote repo created yet).
- **GitHub CLI (`gh`) Resolution:** Least-disruptive host-split workflow adopted — container `git` + SSH (`andycungkrinx91`) for all Git operations; pre-authenticated host `gh v2.104.0` (`andycungkrinx91`) strictly for GitHub API/metadata creation when authorized.
- **Make.fun Connection:** Active (`GET /api/agents/me` -> HTTP `200`, wallet `FRGCRW52gCmUEPbgcD9dr8hjCzjHRNtriiKonmbCTre3`, scopes `enter`, `comment`).
- **Quota Baseline (observed `2026-11-18T16:54Z`):**
  - `can_submit`: `true`
  - Daily cap: `3` (`0` used, `3` remaining, resets `2026-11-19T00:00:00.000Z`)
  - Total cap: `10` (`10` remaining); Hard limit: `20` (`20` remaining)
  - Note: Re-check `/api/competitions/github-core-clone-01/quota` before any submission.

## Competitive Field Notes (Observed `2026-11-18T16:19Z`)

- `submissionCount`: `12,873`
- Top 33 leaderboard entries all achieve `600/600` feature and `200/200` agent (`120/120` checks).
- Paid-place competition is decided by `perf` (`#1: score 966 / perf 166 / 322,015 us`; `#20: score 954 / perf 154 / 377,378 us`), dominated by statically compiled Go or C11 binaries launched from `main.ts`/`main.py`.

## Validation Results

- Local `starter` dry-run via `grade-local --driver process --worktree`: `PASSED` (pipeline verified, `15/1000`).
- Local `starter` dry-run via `grade-local --driver docker --worktree`: `PASSED` (pipeline verified, `15/1000`, live + replay + L1 regrade byte-identical; note CLI host Node `v24.22.2` vs server checkout `v26.13.1`).
- `makefun check` (pre-implementation commit `6472676`): `5 passed` (`competition_open`, `snapshot`, `license_osi`, `entry_size`, `committed_provider_key`), `2 expected pre-code fails` (`entrypoint`, `transcripts_present`), `14 skipped`, `5 checked at submission`.
- `makefun bundle --dry-run`: Requires explicit `--session <transcript_full.jsonl>` when session cwd was opened outside `/data/makefun/github-core-clone-01/`, plus `--harness-version 1.5.11`, `--effort <level>`, and `--system-prompt <file>` (since Antigravity `transcript_full.jsonl` does not embed the system prompt, and entries received from `2026-11-18T12:00Z` require it).
- Entry conformance (`sdk/run.ts`): `NOT RUN` (implementation not started).
- Entry `grade-local`: `NOT RUN` (implementation not started).

## Pending Next Gate & Blockers Before Submission

1. **Implementation Approval:** Ready to implement [ARCHITECTURE_PLAN.md](./docs/ARCHITECTURE_PLAN.md) (`main.ts` + static Go core), run conformance/benchmarks, and create the public GitHub repo under `andycungkrinx91` when authorized.
2. **Transcript Gate Readiness:** Before building/submitting, resolve the `--system-prompt <file>` + `--effort` requirement reported by `makefun bundle --dry-run` (or run build sessions directly inside `/data/makefun/github-core-clone-01/` with exact `--harness-version 1.5.11`, `--effort`, and `--system-prompt`).
