# Codex Cloud CLI — measured contract

Measured 2026-08-03 against `codex-cli 0.146.0`, ChatGPT-subscription auth, one
real run (`task_e_6a70b48f64648323bba8af2747578941`) against environment
`mepuka/codex-cloud-shim`. Raw captures in `docs/probe/`. The implementation
cites these files, never memory. Re-run `docs/probe-plan.md` on CLI upgrades.

## Environment resolution

- `--env` accepts the **repo-slug label** (`owner/repo`) and resolves it when
  unambiguous; wrong labels fail fast: `Error: environment '<x>' not found`.
  The default env label for a repo IS its slug — so the shim derives it from
  `git remote get-url origin` and needs no binding table. An explicit
  `--env-id`/config pin stays as the override for renamed or multiple envs.
- `list --json` reports `environment_id: null` but `environment_label`
  populated — the label is the working identity surface.

## exec — fire-and-forget (probe/exec.time)

- `codex cloud exec --env <label> --branch <b> "<prompt>"` returned in
  **3.4 s**, exit 0. stdout: exactly one line, the task URL
  `https://chatgpt.com/codex/tasks/task_e_<hex>`. stderr: empty.
- Task id = last URL path segment (`task_e_` prefix + 32 hex).
- Consequence: the shim's loop is submit → poll. No streaming from exec.

## States and polling

- Observed lifecycle: `pending` → `ready` (~2 min for a trivial task;
  submit→terminal ≈ 110 s). `status` first line is `[PENDING]` / `[READY]`
  + title; then `env • age`; then diff-stat line (`no diff` or `+3/-0 • 1 file`).
  No `--json` on status — but `list --env <label> --json` carries the same
  state machine-readably:

  ```json
  {"id":"task_e_…","url":"…","title":"…","status":"ready",
   "updated_at":"2026-08-03T15:34:23.8Z","environment_id":null,
   "environment_label":"mepuka/codex-cloud-shim",
   "summary":{"files_changed":1,"lines_added":3,"lines_removed":0},
   "is_review":false,"attempt_total":1}
  ```

  → poll `list --json` (filterable by env, max limit 20) and match by task id;
  reserve `status` for human-oriented detail. Failure-state names NOT yet
  observed — the shim must treat any status ∉ {pending, ready} conservatively
  (report verbatim, don't guess) until a failed run is captured.
- `title` is model-generated from the prompt. The assistant's message text is
  NOT exposed by exec/status/list — progress narration is limited to state +
  diff-stat (+ task URL for the human).

## diff / apply (probe/diff.patch, probe/apply-error.log)

- `diff <id>`: standard git unified diff with full 40-hex index lines, exit 0.
- `apply <id>`: runs `git apply --3way <tmp-patch>` in CWD. Clean tree:
  exit 0, change left **staged** (`git status` shows `A`). Re-apply on an
  already-applied tree: exit 0 (idempotent). Conflicting dirty tree: exit 1,
  `Apply failed … (applied=0, skipped=1, conflicts=0)`, tree left untouched.
  `--attempt <N>` selects among best-of-N.
- **CWD side effect:** the CLI appends to `error.log` in the working directory
  on every cloud invocation — including auth diagnostics (account id) and the
  full apply command line. The shim MUST keep this out of the delivered
  worktree (delete it / `.git/info/exclude`) and must not commit it.

## Auth

- `codex login status` → `Logged in using ChatGPT`; calls hit
  `chatgpt.com/backend-api` path_style=wham with a `ChatGPT-Account-Id`
  header (visible in apply-error.log). The shim never touches auth itself —
  it requires a logged-in `codex` on the host, and preflights with
  `codex login status`.

## Open items (need a future capture)

- Failure-state vocabulary (a run that errors upstream).
- `--attempts N > 1`: list/apply shapes for sibling attempts.
- Behavior when the env label is ambiguous (two envs, same repo).
- Whether `exec` can target a branch that doesn't exist upstream.
