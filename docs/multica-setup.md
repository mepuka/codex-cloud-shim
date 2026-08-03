# Using Codex Cloud from Multica — setup and per-project onboarding

State as wired on 2026-08-03, workspace `dev`.

## One-time platform setup (done)

1. **Shim on the always-on host only.** `go install
   github.com/mepuka/codex-cloud-shim/cmd/codex-cloud-shim@latest` on the PC.
   Deliberately NOT on the mac: the runtime profile registers wherever the
   command resolves, and a sleeping machine holding a poll kills the run's
   tracking. The mac's profile row showing `offline` is the design working.
   Tracked in the `mepuka/tools` manifest (hosts: pc).
2. **Runtime profile** (workspace-visible, so every project can use it):
   `multica runtime profile create --display-name "Codex Cloud"
   --protocol-family claude --command-name codex-cloud-shim` — profile
   `7e26ff20`, registered as runtime `Codex Cloud (mepuka-pc)` (`e9d78a72…`).
3. **Agent `Cloud Codex`** (`5459089f…`) on that runtime with
   `custom_args: ["--shim-land","push"]` — inside Multica the platform's
   delivery path is commit-and-push-own-branch, so push mode is correct there
   (the standalone default stays `commit`).
4. **`codex` logged in with ChatGPT on the PC** (`codex login status`).

## Per-project onboarding — the only recurring steps

1. **Create the repo's Codex Cloud environment** — the one step with no API
   (creation lives only in the web UI): chatgpt.com/codex → settings →
   environments → pick the repo, defaults are fine. Env label = `owner/repo`,
   which is exactly what the shim derives from the git origin — no binding
   config needed.
2. **Verify:** `codex-cloud-shim env check owner/repo` (or from inside the
   checkout with no argument). It resolves via the local codex login; when no
   env exists it prints the create link. A task fired against a repo with no
   env fails fast with the same guidance (F4a).
3. **Register the repo in the WORKSPACE registry** — `multica repo add
   <https url>`. This is the step that makes the daemon clone a checkout into
   task workdirs; a project-level `--repo` resource alone is metadata and the
   task will fail E_GIT_CONTEXT "no checkout was found" without the registry
   entry (measured on DEV-87, 2026-08-03). Then attach the repo to the
   project and assign issues to **Cloud Codex**.
4. **Trigger semantics** (measured): assignment at issue creation fires the
   agent immediately; a failed run is re-fired by a comment MENTIONING the
   agent (it replies threaded under the trigger); flipping the status column
   re-fires nothing.

That's it: tailtalk needs only step 1 (create its env) to start using cloud
runs.

## How a run flows

Issue assigned to Cloud Codex → PC daemon claims → shim submits
`codex cloud exec --env <origin slug> --branch <base>` (fire-and-forget,
~3 s) → polls `list --json` (~2 min for small tasks) → applies the diff into
the task worktree, commits with `Codex-Cloud-Task/-URL/-Base` trailers, pushes
the agent branch. The transcript carries the cloud task id as session_id from
the first event, so a retry reconciles instead of double-submitting; the
upstream run is never cancelled by local failures.

## Knobs (per agent, via custom_args)

`--shim-land report|apply|commit|push`, `--shim-attempts N` (best-of-N),
`--shim-deadline` (default 30m), `--shim-poll-interval` (30s),
`--shim-env` / `--shim-branch` overrides, `--shim-frame cloud|off` (default
`cloud`: wrap the platform brief in a cloud-facing preamble AND materialize
the issue's title/description via `multica issue get` — ownership-mode
prompts carry no task content at all, only a pointer the cloud model cannot
follow, the measured empty-diff failure of the first live runs). After a
successful ownership-turn landing the shim moves the issue to `in_review`
(best-effort, never `done`). A reviewer-flavored agent is just a
second agent on the same runtime with different custom_args (e.g.
`["--shim-land","report","--shim-attempts","3"]`).
