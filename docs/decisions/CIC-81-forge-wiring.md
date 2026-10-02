## [CIC-81] Forge wiring: checks on run terminal, forge re-runs (2026-10-02)

Before this change the server always used `NoOpForge`, `forge.Complete` had no
caller, and `check_suite` / `check_run` re-run requests were logged and dropped.

### Decision

- **Forge selection.** `--github-app-token` (env `CICADO_GITHUB_APP_TOKEN`)
  selects the GitHub Checks forge; empty keeps `NoOpForge`. No token minter is
  wired, so a static installation token (1 hour lifetime) suits dev servers
  only. Per-call installation-token minting is CIC-146. The Checks API
  accepts only GitHub App installation tokens (a PAT cannot create check
  runs), so unlike CIC-120's workflow-fetch token there is no PAT fallback.
- **Check ID.** `controller.report` records the ID `forge.Prepare` returns in
  `runs.prepared_check_id` (migration 0004, nullable). NULL means "no check":
  NoOp forge, a failed Prepare, or a run older than the migration. The column is
  read only by `store.GetRunCheckRef` (into `sql.NullInt64`), never by
  `runSelectColumns`, so the runs API cannot trip over the NULLs.
- **Completion on run terminal.** `Controller.RunCompleted` is called from the
  single `runnersvc.Config.OnRunTerminal` hook that CIC-103 (#99) added. The
  queue fires it once per run, after the commit that finished the run's last
  job (runner report, sweeper dead letter, cancel scan alike), so the check is
  completed with the **run's** outcome, never a single job's. main.go sets the
  hook before `runnersvc.New` copies the config, and runs the HTTP round trip on
  its own goroutine with a 30 s timeout because the hook must not block. A run
  with no recorded check is skipped rather than completing check 0.
- **Check prepared before jobs.** `report()` (Prepare + record the ID) runs
  right after `CreateRun`, before `LoadAndCreateJobs` enqueues anything. If it
  ran after, a fast run could reach `RunCompleted` before its check ID was
  recorded, skip `Complete`, and leave the check in_progress forever, which
  blocks merging when the check is required.
- **Runs with no jobs.** A run that gets no jobs (no match, no loader, load
  error) never finishes a job, so `OnRunTerminal` never fires for it.
  `finishIfNoJobs` completes its check right after `LoadAndCreateJobs`: with the
  run's terminal status when CIC-85 has finalised it, otherwise `failed` on a
  load error and `succeeded` on no match. If the job count cannot be read the
  check is left open rather than completed early.
  `succeeded`→`success`, `failed`→`failure`, `cancelled`→`cancelled` (a
  conclusion GitHub supports; added to `forge` and `github`).
- **Re-runs.** `check_suite` `rerequested` and `check_run` `rerequested` create
  a new run that replays the most recent run at that commit
  (`store.FindRerunSource`): its event, ref, fork flag, clone URL and checkout
  ref. `check_suite` `requested` is ignored: GitHub sends it for every push and
  HandlePush has already created that run.

### Why replay the original run

- `check_suite` / `check_run` are not workflow trigger types; matching needs the
  original `push` / `pull_request` event and ref.
- Neither payload carries a fork signal. The original run's `is_fork` is the only
  trustworthy one, so a re-run of a fork PR stays a fork run (no secrets, no
  deploy identity). A commit CICaDo never ran is skipped rather than guessed.
- The PR action is not stored. A `pull_request` re-run is replayed as
  `synchronize`, the action GitHub sends when a PR's head changes. A workflow
  restricted to `types: [opened]` therefore does not re-run; that is the
  conservative failure.

### Not in this PR

Holding the backpressure slot until the run is terminal (and releasing it from
the same hook) is CIC-106 (#102). The re-run path uses the same
acquire/deferred-release pattern as `HandlePush` so CIC-106 changes all of them
together.
