# Retire custom video and adopt upstream Seedance

## Goal and approved scope

Remove the self-developed video platform completely from executable code, API routes, administration, pricing, billing holds, provider adapters, task storage, workers and UI. Merge upstream main at b8dece900 (v0.2.13), retaining its native Seedance and Grok media behavior. The user explicitly confirmed custom video has no consumers.

## Architecture and constraints

- Native Seedance uses upstream `/contents/generations/tasks` routes and OpenAI/composite accounts, not the retired `/videos` implementation.
- Preserve unrelated local extensions: Feishu, capture-only prompt audit, OpenAI instruction privacy, Ark compatibility, request diagnostics and image routing.
- Do not remove or rewrite already-applied migration history, purge business data, push or deploy.
- Preserve existing `.superpowers/` and the separate unified-video worktree. This document also serves as the execution ledger instead of using that shared scratch directory.
- Historical video data may remain in SQL tables, but no runtime component reads, writes, schedules or exposes the custom subsystem.

## Tasks

1. Merge upstream in an isolated worktree; resolve shared-code conflicts without losing unrelated local customizations.
2. Remove custom video-only source files and shared hooks; restore upstream native media paths; regenerate Ent and Wire.
3. Verify legacy migration compatibility and native Seedance settlement. Add a regression for mandatory settlement when the ordinary usage queue overflows; write and run the failing test before fixing it.
4. Run backend and frontend checks, review the full change, fix important findings and report the result. No remote publication in this request.

## Review focus

Custom video symbol leakage, regenerated schema correctness, applied migration checksums, legacy platform rows, native Seedance billing dispatch, unrelated local behavior retained across conflicts, and route/UI removal without breaking native Grok/Seedance.

## Ledger

- Worktree created from main 487972702. Native worktree tool could not identify the nested repository; used Git fallback under ignored `.worktrees/`.
- Pre-flight: tasks 1 and 2 share video/group/platform contracts; upstream is authoritative for native media, unrelated local deltas remain authoritative for custom extensions.
- Pre-flight: tasks 2 and 3 share SQL history; retain immutable historical migrations and permit legacy platform rows in new constraints without restoring runtime support.
- Ruling: no historical table or record deletion — the request retires the implementation, not production business data; deleting history would break migration checksums and safe upgrades.
- Tasks 1–2: upstream v0.2.13 merged in the isolated worktree; custom video API/admin/provider/task/hold/worker/UI paths removed; Ent and Wire regenerated. Native Seedance and Grok routes retained.
- Red/green: legacy custom content endpoints returned 503 before retirement and 404 after retirement. Native route regression suite passes.
- Red/green: native Seedance settlement dropped on worker-pool overflow; realistic hashed billing RequestID plus `seedance:` ResponseID regression now passes with mandatory dispatch.
- Ruling: adopt upstream backup record storage to retain monthly archive and restore metadata; new migration 242 imports the authoritative legacy table without changing old migrations. Preserve extra JSON metadata for live IDs, but do not resurrect deleted IDs from the stale settings snapshot. Real PostgreSQL regressions verified both cases, repeat application and corrupt-history rejection.
- Ruling: keep fleet operation locking, local operation checks, periodic schedule reload and encrypted-secret preservation alongside upstream backup record writer locks. Recovery/restore tests exposed dropped merge guards; guards restored. DB-only nested locking rejects bounded pools below three connections before locking; isolated writer-only use still supports two.
- Review: read-only whole-change review found no Critical issue. Removed the reported dead composite route helper; fixed the conditional DB-only lock-pool issue. Broader unchanged upstream behavior and local-extension internals were not exhaustively re-audited, but integration seams and full unit suites are verified. Historical-data reconciliation, native Redis binding expiry/poll-triggered billing and live-provider deployment testing remain outside this code-retirement request.
- Verification: frontend 336 files / 2619 tests and production build passed; real PostgreSQL upgrade/checksum compatibility suite passed; backend embedded build passed. Final `go test -tags=unit ./...` exited 0, including the guarded service suite (197.052s). Final focused review reports no remaining Critical, Important or Minor findings. Whitespace checks and custom-runtime-symbol searches are clean; the main checkout remains unchanged except its pre-existing untracked `.superpowers/`.
- Completion gate: code retirement and upstream integration are ready on `codex/retire-custom-video`; no remote push, production deployment, historical data purge or native live-provider test was performed. Await the user's integration choice before touching main or publishing.
