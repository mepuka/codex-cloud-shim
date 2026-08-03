# codex-cloud-shim — authoritative design

Synthesized from two drafts (A: protocol-correctness, B: operational-simplicity)
against the verified specs. Sources for every behavioral claim:
`docs/multica-claude-contract.md` (Multica parser, `multica@8df7549
server/pkg/agent/claude.go`), `docs/codex-cloud-contract.md` (codex-cli
0.146.0, measured), `docs/probe/*` (raw captures, cited per-file). Go 1.25,
stdlib only, module `github.com/mepuka/codex-cloud-shim`, one static binary
`codex-cloud-shim`.

## Conflicts resolved (A vs B)

| # | Topic | Resolution |
|---|---|---|
| C1 | Exit codes | A wins: `0` success result, `1` error result, `143` cancel (SIGTERM / stdin-EOF-before-result). B's always-exit-0 discarded — the claude CLI the daemon expects exits nonzero on error, and 143 is the conventional SIGTERM code. |
| C2 | Retry reconciliation | Both: A's **marker file** (git-dir, prompt hash) is the cross-process memory; the aliveness check uses **`list --json` matched by id** (the fixed decision's wording) with paginate ≤ `--shim-list-pages`, falling back to `codex cloud status <id>` bracket-parse only when the id has aged off the pages. B's pre-submit **snapshot + orphan adoption** is kept for the intra-run exec-failure window (the exact duplicate-submission hazard). |
| C3 | Resume semantics | A wins: `--resume` splits on the marker's prompt hash — same hash = retry (adopt), different hash or no marker = **follow-up** (new task, prefixed prompt). B's attach-always would silently drop a follow-up prompt. |
| C4 | Unknown cloud status | Merged: keep polling and narrate verbatim (B), but terminate early with F8 when the same unknown status shows an **unchanged `updated_at` for 5 consecutive polls** (A, threshold widened). `terminalFailureStatuses` starts deliberately empty (B) pending a captured failed run (codex-cloud-contract §Open items). |
| C5 | `log` events | B wins: emit `log` events for diagnostics (env/branch decisions, hygiene actions, orphan adoption). The daemon forwards them (multica-claude-contract event table). |
| C6 | report-mode diff | A wins: `report` mode fetches `codex cloud diff <id>` and embeds it (1 MiB truncation). B's stats-only report defeats the mode's purpose. `diff` runs in scratch CWD, so no hygiene exposure. |
| C7 | error.log hygiene | Merged: scratch-CWD quarantine for every codex call except `apply` (A+B), plus B's wrap around apply — stat-before (absent \| size N), idempotent `.git/info/exclude` entry, restore-after (absent→`rm -f`; pre-existing→truncate to N; tracked-and-modified→`git checkout -- error.log` (A)). Never `git add`/`-a`, so the commit is safe even if restore races. |
| C8 | Commit message | B's trailers (`Codex-Cloud-Task/-URL/-Base`) via `git commit -F -`; title = cloud `title` + ` (KEY)`, fallback first prompt line ≤60 runes (B), ultimate fallback `codex-cloud: <id>` (A). Issue-key regex is B's tighter form; A's `--shim-issue-key` override kept. |
| C9 | Idempotent re-land | B wins: `git log --grep="Codex-Cloud-Task: <id>"` pre-check + apply's measured idempotency make every retry safe at any interruption point. |
| C10 | Preflight auth check | Exit code of `codex login status` only; no stdout phrase-match (B's `Logged in` grep is brittle across CLI versions). Stdout is quoted into the failure text verbatim. |
| C11 | codex binary flag | `--shim-codex-bin` (A's name), env fallback `CODEX_CLOUD_SHIM_CODEX_BIN`, flag wins. |
| C12 | Prompt size guard | B wins: prompt > 100 KiB → config error (it becomes an argv element for `exec`; stay far under ARG_MAX). |
| C13 | Clock vs tiny durations in tests | Both: `run` takes a `Clock` interface for cadence-precision unit tests; integration tests drive real time with tiny flag values (`--shim-poll-interval 20ms` …). |
| C14 | Unknown-flag arity | Ignore `--x=y` whole; bare unknown `--x` treated as boolean, consumes nothing; one `log` warn each (B's no-consume rule — guessing arity can eat a real token; today's Multica launch line contains no unknown value-taking flags, claude.go:668). |

---

## 1. Package layout

```
codex-cloud-shim/
├── cmd/codex-cloud-shim/main.go   # wiring only: argv → config, signal/stdin watchers, os.Exit
├── internal/argv/argv.go          # claude-argv scanner (arity tables) + --shim-* flags
├── internal/proto/
│   ├── events.go                  # event structs, Emitter (serialized NDJSON, result latch, phrase guard)
│   └── stdin.go                   # prompt-line reader + background drainer
├── internal/prompt/prompt.go      # prompt text extraction + issue-key regex (pure)
├── internal/cloud/
│   ├── client.go                  # codex subprocess adapter (login/exec/list/status/diff/apply)
│   └── types.go                   # list --json structs
├── internal/gitx/gitx.go          # origin slug, base branch, commit, push, error.log hygiene
├── internal/state/state.go        # reconcile marker file in git-dir
├── internal/run/
│   ├── run.go                     # state machine: Run(ctx, deps, cfg) → exitCode
│   ├── land.go                    # landing modes report/apply/commit/push
│   ├── taxonomy.go                # failure codes → result mapping, resume-reject phrase constant
│   └── clock.go                   # Clock interface (real + fake)
└── internal/testsupport/          # test-only: stub codex writer, scenario builder, git fixture
    └── stub.go, gitfixture.go, testdata/…
```

Dependency direction: `run` → {`argv`,`proto`,`prompt`,`cloud`,`gitx`,`state`};
nothing imports `run`. No package-level mutable state; `context.Context` flows
through every subprocess call; errors wrapped `%w` with the argv.

---

## 2. CLI surface

### 2.1 Claude-compatible argv (accepted, ignored)

Multica launches (`buildClaudeArgs`, claude.go:668):

```
codex-cloud-shim -p --output-format stream-json --input-format stream-json --verbose \
  --permission-mode bypassPermissions --disallowedTools AskUserQuestion \
  [--strict-mcp-config] [--model M] [--effort E] [--max-turns N] \
  [--resume SESSION_ID] [extra_args...] [--shim-* custom_args...] [--settings PATH]
```

plus possibly `--mcp-config <path>` (claude.go:65). Stdlib `flag` stops at the
first unknown token, so `argv.Parse` is a hand-rolled single-pass scanner with
arity tables:

| value-taking (consume next token unless `=` form) | boolean |
|---|---|
| `--output-format` `--input-format` `--permission-mode` `--disallowedTools` `--allowedTools` `--model` `--effort` `--max-turns` `--resume` `--settings` `--mcp-config` `--append-system-prompt` `--system-prompt` `--session-id` `--fallback-model` `--add-dir` `--permission-prompt-tool` | `-p` `--print` `--verbose` `--strict-mcp-config` `--continue` `--dangerously-skip-permissions` `--include-partial-messages` |

Only `--resume <id>` is consumed with meaning (§5.4). Positionals are ignored
(the prompt arrives on stdin). Unknown flags: per C14. The tables are data;
extending them for a future claude flag is one line plus a table-test row.

`--version` → print `codex-cloud-shim v0.1.0`, exit 0, touching nothing (the
daemon version-probes at registration, daemon.go:1719–1821; nonzero would
deregister the runtime). `--help` → usage, exit 0.

### 2.2 Shim flags (`--shim-*`, arrive after claude flags via Multica `custom_args`)

Both `--shim-x v` and `--shim-x=v` forms. Durations via `time.ParseDuration`;
malformed values → F0.

| Flag | Default | Meaning |
|---|---|---|
| `--shim-env <label-or-id>` | derived from origin slug (§7.1) | passed verbatim as `--env` |
| `--shim-branch <b>` | detected (§7.2) | base branch; `--branch` is ALWAYS passed to exec |
| `--shim-land <mode>` | `commit` | `report` \| `apply` \| `commit` \| `push` |
| `--shim-frame <mode>` | `cloud` | `cloud` wraps the submit prompt in the cloud-facing preamble (§5.5); `off` submits the stdin prompt verbatim |
| `--shim-attempts <N>` | unset | forwarded as `exec --attempts N` |
| `--shim-attempt <N>` | unset | forwarded as `apply --attempt N` |
| `--shim-poll-interval <dur>` | `30s` | list-poll cadence |
| `--shim-keepalive <dur>` | `2m` | max silence between emitted events while polling |
| `--shim-deadline <dur>` | `30m` | overall wall-clock budget, launch → result |
| `--shim-exec-timeout <dur>` | `120s` | budget for the exec subprocess (measured 3.4 s — probe/exec.time) |
| `--shim-issue-key <KEY>` | extracted from prompt (§7.3) | commit-title suffix `(KEY)` |
| `--shim-codex-bin <path>` | `codex` (env `CODEX_CLOUD_SHIM_CODEX_BIN`, flag wins) | the codex executable; the test seam. MUST resolve to a native executable, never npm's `codex.cmd`: a batch shim runs through cmd.exe, whose command line dies at the first newline, silently truncating the prompt to line 1 (measured 2026-08-03 — five live cloud runs returned empty diffs titled after the prompt's first line; the identical prompt through the vendor `codex.exe` landed +12/-0). PREFLIGHT fails F2 on a `.cmd`/`.bat` resolution. |
| `--shim-list-pages <N>` | `5` | max cursor pages when hunting a task id in `list` |

---

## 3. Subprocess discipline (the error.log rule)

Measured: the CLI appends `error.log` to its CWD on **every** cloud invocation,
including auth diagnostics with the account id (probe/error.log) and the full
apply command line (probe/apply-error.log). Fixed decision: never delivered.

- Every codex invocation runs with `cmd.Dir` = a per-run scratch dir
  (`os.MkdirTemp`, removed on exit) — **except `codex cloud apply`**, which
  must run in the worktree because it executes `git apply --3way` in CWD
  (probe/apply-error.log `cmd=`). The scratch dir is made a minimal git repo
  mirroring the worktree's origin URL (`gitctx.MirrorOrigin`): every probe
  capture was taken from inside a checkout and the raw record shows the CLI
  parsing the CWD git origin during env resolution (probe/error.log); a bare
  non-repo CWD is unmeasured (probe-plan open items).
- Apply is wrapped by `gitx.WithErrorLogHygiene` (C7): before — stat
  `<worktree>/error.log` (absent | size N | tracked), idempotently append
  `error.log` to `.git/info/exclude` (never `.gitignore`: the delivered tree
  must not change); after, success or failure (the failed apply also wrote it —
  probe/apply-error.log): absent-before → `rm -f`; untracked-present →
  truncate to N; tracked-modified → `git checkout -- error.log`. Each action
  emits a `log` event.
- Belt-and-braces: the shim never runs `git add` or `commit -a`; apply stages
  only patch paths, so error.log cannot enter a commit even if restore fails.
- Hard timeouts via `exec.CommandContext`: `--shim-exec-timeout` for exec,
  60 s for list/status/login, 300 s for diff/apply. `cmd.WaitDelay = 5s` so a
  wedged child never wedges the shim. No shell anywhere; the prompt is one
  argv element.

---

## 4. Emitted events — exact JSON

### 4.1 Emitter invariants

- One event per line: `json.Marshal` + `"\n"`, single `Write` to stdout under
  a mutex. Max 10 MB/line daemon-side (claude.go event loop); the only large
  field (report-mode diff) is truncated to 1 MiB + `\n[truncated]`.
- **Result latch**: after `result` is written the emitter permanently refuses
  writes. Exactly one `result`, always last (claude.go:176–303 treats it as
  terminal; daemon closes stdin on seeing it).
- `session_id` = the Codex Cloud task id (fixed decision), present on every
  event once known, `omitempty` before then. Never emitted empty or bogus:
  the daemon stores the result-reported id and feeds it back as `--resume`.
- **Never emitted**: `user` (any `status:"async_launched"` inside one fails
  the whole run — claude.go table) and `control_request` (would trigger
  daemon control_response writes). Unit test asserts the emitter has no
  constructor for either.
- **Phrase guard**: the resume-reject phrase (§4.2 E8) may appear only in the
  E8 template. All upstream stderr quoted into any other event is filtered —
  any reject-phrase substring is replaced with `[redacted-resume-phrase]` —
  so an incidental match can never fire the daemon's fresh-retry lever.

Structs (field order = wire order; golden tests are byte-stable):

```go
type TextBlock struct { Type string `json:"type"`; Text string `json:"text"` } // "text"
type Message struct {
    Role    string      `json:"role"`    // "assistant"
    Model   string      `json:"model"`   // "codex-cloud"
    Content []TextBlock `json:"content"`
}
type LogBody struct { Level string `json:"level"`; Message string `json:"message"` }
type Event struct {
    Type       string   `json:"type"`               // "system"|"assistant"|"log"|"result"
    Subtype    string   `json:"subtype,omitempty"`  // system:"init"; result:"success"|"error_during_execution"
    SessionID  string   `json:"session_id,omitempty"`
    Message    *Message `json:"message,omitempty"`  // assistant only
    Log        *LogBody `json:"log,omitempty"`      // log only
    DurationMS int64    `json:"duration_ms,omitempty"` // result only
    NumTurns   int      `json:"num_turns,omitempty"`   // result only, always 1
    IsError    *bool    `json:"is_error,omitempty"`    // result only (pointer: false must serialize)
    Result     string   `json:"result,omitempty"`      // result only
}
```

No `usage`/`modelUsage` ever: usage is accumulated only when present with a
non-empty model, cloud exposes no token counts, and omission is well-formed
(multica-claude-contract §Output).

### 4.2 Event catalog

**E1 — system init** (REQUIRED early; daemon captures `session_id`, flips
status to "running"). Emitted the moment the task id is known — after exec
parses on fresh runs (~3.4 s), immediately on adopt/resume:

```json
{"type":"system","subtype":"init","session_id":"task_e_6a70b48f64648323bba8af2747578941"}
```

**E2 — assistant: submitted** (immediately after E1; text varies:
`Submitted…` / `Adopted existing…` / `Resuming…`):

```json
{"type":"assistant","session_id":"<ID>","message":{"role":"assistant","model":"codex-cloud","content":[{"type":"text","text":"Submitted Codex Cloud task <ID> (env mepuka/codex-cloud-shim, base branch main).\nhttps://chatgpt.com/codex/tasks/<ID>\nPolling every 30s; deadline 30m."}]}}
```

**E3 — assistant: state change** (whenever a poll observes a status different
from the last emitted one; diff-stat clause from list `summary`, omitted when
all-zero — probe/list-1785771264.json):

```json
{"type":"assistant","session_id":"<ID>","message":{"role":"assistant","model":"codex-cloud","content":[{"type":"text","text":"Task <ID> status: ready (+3/-0, 1 file). Elapsed 1m50s."}]}}
```

**E4 — assistant: keepalive** (fires only when no other event was emitted
within `--shim-keepalive`; any emit resets the timer, so cadence is "at least
one event per window" — satisfies `SemanticInactivityTimeout` /
`IdleWatchdogTimeout`, multica-claude-contract §Liveness):

```json
{"type":"assistant","session_id":"<ID>","message":{"role":"assistant","model":"codex-cloud","content":[{"type":"text","text":"Still waiting on Codex Cloud task <ID>: status pending, elapsed 4m0s, deadline in 26m0s."}]}}
```

**E5 — assistant: landing** (one line before landing work in
apply/commit/push; watchdog cover through a slow apply):

```json
{"type":"assistant","session_id":"<ID>","message":{"role":"assistant","model":"codex-cloud","content":[{"type":"text","text":"Task ready; landing with mode commit (apply → commit)."}]}}
```

**E6 — log** (diagnostics, forwarded by the daemon; used for env/branch
decisions, hygiene actions, orphan adoption, unknown-flag warnings;
`level` ∈ `info|warn|error`):

```json
{"type":"log","session_id":"<ID-if-known>","log":{"level":"info","message":"base branch main (rule: upstream of HEAD)"}}
```

**E7 — result: success** (exactly one, last):

```json
{"type":"result","subtype":"success","session_id":"<ID>","duration_ms":123456,"num_turns":1,"is_error":false,"result":"Codex Cloud task <ID> completed.\nTitle: Create docs/PROBE.md with UTC date\nURL: https://chatgpt.com/codex/tasks/<ID>\nDiff: +3/-0 across 1 file\nLanded: commit 4a5b6c7 on agent/x/y — \"Create docs/PROBE.md with UTC date (DEV-123)\"\nFiles:\nA\tdocs/PROBE.md"}
```

`Landed:` line by mode — `report`: `report only (no worktree changes)` plus
the full unified diff fenced under `Diff:` (1 MiB truncation); `apply`:
`applied and staged (not committed)`; `commit`: as above; `push`: adds
`Pushed: origin/<branch>`. Empty diff (any mode): `no changes (empty diff)`,
no apply/commit. Idempotent re-land: `already landed as <sha> (no-op)`.

**E8 — result: error** (generic failure; every taxonomy row except
`R_RESUME_NOT_FOUND`):

```json
{"type":"result","subtype":"error_during_execution","session_id":"<ID-if-known>","duration_ms":8000,"num_turns":1,"is_error":true,"result":"<first line per taxonomy code>\nTask: <ID> https://chatgpt.com/codex/tasks/<ID>\nThe upstream Codex Cloud run was NOT cancelled and may still complete; a retry will reconcile by task id."}
```

`session_id` is **omitted** when no task exists yet (pre-submit failures) so
no bogus id enters the daemon's resume pointer; it is **set** on every
post-submit failure so the retry arrives as `--resume <id>` and adopts
instead of double-submitting. Upstream stderr is quoted ≤ 2000 bytes,
reject-phrase-filtered (§4.1).

**E9 — result: resume-reject** (the deliberate lever — a phrase match against
`resumeRejectedPhrases` makes the daemon drop the session pointer and retry
fresh; multica-claude-contract §Session identity). Emitted only by
`R_RESUME_NOT_FOUND`; no `session_id` (the pointer is being dropped):

```json
{"type":"result","subtype":"error_during_execution","duration_ms":2500,"num_turns":1,"is_error":true,"result":"No conversation found with session ID: <ID>. The Codex Cloud task id could not be found upstream; start a fresh session."}
```

```go
// taxonomy.go — must literally match an entry in Multica's
// resumeRejectedPhrases (server/pkg/agent/claude.go:769 @ 8df7549).
// Pre-implementation verification V1: byte-check against the source
// before freeze; only this constant changes if it differs.
const resumeRejectPhrase = "No conversation found with session ID"
```

---

## 5. State machine

```
INIT ──F0──▶ RESULT(err, no sid)
 ▼
READ_PROMPT ──F1──▶ RESULT(err, no sid)
 ▼
PREFLIGHT ──F2──▶ RESULT(err, no sid)
 ▼
RESOLVE(env, branch) ──F3──▶ RESULT(err, no sid)
 │
 ├─ --resume given ─▶ RESUME_ROUTE ──R_RESUME_NOT_FOUND──▶ RESULT(E9)
 │                      ├─ retry, id alive ────────▶ ADOPT
 │                      └─ follow-up ──────────────▶ SNAPSHOT
 ▼
RECONCILE (marker) ── marker id alive ─▶ ADOPT
 │ (no marker / id dead → delete marker)
 ▼
SNAPSHOT ─▶ SUBMIT ──F4/F4a/F4b──▶ RESULT(err, no sid)   [orphan-adopt may rescue → ADOPT]
 │ write marker; ADOPT joins here
 ▼
EMIT E1 + E2
 ▼
POLL ── every interval; E3 on change; E4 keepalive
 │   ├─ F5 deadline ─────▶ RESULT(err, sid)
 │   ├─ F7 poll failures ▶ RESULT(err, sid)
 │   ├─ F8 unknown+stall ▶ RESULT(err, sid)
 │   └─ CANCEL ──────────▶ exit 143, no result, upstream untouched
 ▼ status == "ready"
LAND (--shim-land) ──F9..F12──▶ RESULT(err, sid)
 ▼
RESULT(E7) ─▶ exit 0
```

`run.Run` is a loop over `func(ctx, *runState) (state, error)` handlers so
every transition is a return value and unit-testable.

### 5.1 INIT / READ_PROMPT / stdin & signals

- Parse argv (F0: malformed shim flag → error result, no sid). Install signal
  handler, create scratch dir, start the deadline timer, record start time.
- Read exactly one stdin line (`bufio.Reader`, 10 MiB cap, 60 s guard — the
  daemon writes the prompt immediately, `buildClaudeInput` claude.go:739):
  `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<prompt>"}]}}`.
  `content` accepted as array of text blocks (concatenate in order, joined
  `"\n"`) or bare string. Empty text, EOF, timeout, malformed JSON → F1.
  Prompt > 100 KiB → F1 (C12).
- **Drainer**: a goroutine keeps reading stdin and discards everything (we
  never emit `control_request`, so nothing meaningful arrives; draining
  prevents daemon-side write blocking). Stdin **EOF before result** =
  cancellation (daemon closes stdin on cancel, claude.go:192–213) → CANCEL:
  stop timers, no landing, upstream run untouched (fixed decision — the id is
  already in the transcript via E1–E4), emit nothing further, exit 143 within
  2 s. A `resultEmitted` atomic gates this: EOF **after** result is the
  daemon's normal close and is ignored.
- SIGTERM/SIGINT → same CANCEL path. The context cancels child processes
  (killing a local `list` in flight cannot touch the cloud run — it lives
  server-side). EPIPE on stdout during CANCEL is ignored.

### 5.2 PREFLIGHT / RESOLVE

- `LookPath(codex-bin)` miss → F2. `codex login status` (scratch CWD) exit
  ≠ 0 → F2 with verbatim output ≤ 2000 bytes (C10). The shim never touches
  auth (codex-cloud-contract §Auth).
- Env: `--shim-env` wins; else origin slug (§7.1). Unresolvable → F3. A wrong
  label fails fast downstream (`Error: environment '<x>' not found`) → F4a
  verbatim.
- Branch: `--shim-branch` wins; else the §7.2 chain. Exhausted → F3 with an
  instruction to pin `--shim-branch` via custom_args. Both decisions emitted
  as E6 log events.

### 5.3 RECONCILE / SNAPSHOT (fresh runs)

Fixed decision made structural: on retry, reconcile via list before
submitting a duplicate. Marker file (survives shim death, never delivered):
`$(git rev-parse --git-dir)/codex-cloud-shim/state.json`:

```json
{"task_id":"task_e_…","url":"…","prompt_sha256":"…","env":"owner/repo","branch":"main","land":"commit","submitted_at":"2026-08-03T15:32:29Z"}
```

Written and fsynced immediately after exec output parses, **before** E1.
Fresh launch with a marker whose `prompt_sha256` matches this prompt: check
aliveness via `list` (id-matched, ≤ `--shim-list-pages` pages; `status <id>`
bracket-parse fallback if aged off). Alive → **ADOPT** (E1 + E2 "Adopted
existing…", straight to POLL; `ready` short-circuits to LAND). Dead → delete
marker, submit fresh. Corrupt marker = no marker (log warn only).
**"Dead" requires a positively-identified not-found**: the shape of
`codex cloud status <dead-id>` is uncaptured (probe-plan open items) and
Status also errors on timeouts and unparsable banners, so any status failure
during the aliveness check is an F7 poll failure (E8 with the id) — never a
gone-verdict; the marker is never deleted on an errored check.

**SNAPSHOT**: one `list` recording existing task ids + `time.Now()` as
`submitMark`. Failure is non-fatal (log warn, empty set) — it only weakens
orphan adoption.

### 5.4 RESUME_ROUTE (`--resume <ID>` present)

codex-cli 0.146.0 has no follow-up/reply verb (exec/list/status/diff/apply
only), so "resume" routes on the marker's prompt hash:

- **Retry** (marker exists, hash matches, marker id == resume id): aliveness
  check as §5.3. Alive → ADOPT. Positively not found → **R_RESUME_NOT_FOUND**
  → E9; the daemon drops the pointer and retries fresh (that fresh run
  re-enters RECONCILE — this is the only path where a fresh submit is
  known-safe, because list just proved the task gone). Per §5.3, a status
  failure is NOT "not found": it is F7 (E8 with the id), so the E9 branch is
  dormant until the dead-id status shape is captured.
- **Follow-up** (hash differs, or no marker): submit a NEW task. Prompt sent
  upstream = `Follow-up to Codex Cloud task <old-ID>; earlier changes for
  this task may already exist on the branch.\n\n<new prompt>` (prefix only
  when a marker existed to name the old task). Branch: in `push` mode, if
  `git ls-remote --heads origin <current-branch>` shows the agent branch
  upstream, use it (cloud builds on delivered work); else the §5.2 base. The
  new task id becomes `session_id`; the daemon stores it from our result — by
  design one Multica session maps to a chain of cloud task ids, latest always
  resumable. Known edge (accepted): a retry whose marker was lost routes as
  follow-up and duplicates once — no mechanism can distinguish it, and the
  E9 alternative duplicates too, one run later.

### 5.5 SUBMIT

`codex cloud exec --env <E> --branch <B> [--attempts N] <prompt>` — scratch
CWD, `--shim-exec-timeout`.

Prompt frame (`--shim-frame`, default `cloud`): the submitted prompt is the
stdin prompt (plus the §5.4 follow-up prefix when present) wrapped in a
cloud-facing preamble that names the env and base branch, tells the model the
brief may address a local agent whose platform mechanics (checkouts, comment
replies, issue-status moves) do not apply, and that the deliverable is file
changes, not prose. Measured motivation: the first live in-Multica run
(2026-08-03) submitted the platform brief verbatim and produced an empty diff
— the cloud model answered the local-agent instructions conversationally.
Issue materialization (same measured motivation, third live run): the
daemon's ownership-mode prompt carries **no task content at all** — multica @
37f3bb7 `buildPromptBody` names the issue id and instructs the agent to run
`multica issue get` itself, which the cloud model cannot do. So when the
frame is on, the shim resolves the issue id (the
`.multica/daemon_task_context.json` sidecar at the workdir root first, the
prompt's stable `Your assigned issue ID is:` line as fallback), runs
`multica issue get <id> --output json` (60 s budget, workdir CWD), and leads
the framed task with the issue's identifier/title/description; the raw
dispatch message follows under a "provenance only" divider. Failure policy:
an ownership turn whose issue cannot be materialized fails **F13** (a
contentless submit is a paid no-op upstream); reply turns carry their trigger
comment inline (`buildCommentPrompt`) and degrade with a warning; a prompt
with no issue id at all (non-Multica usage) submits unenriched.

The frame and issue block are applied at exec time only; the reconcile
marker's `prompt_sha256` stays the hash of the raw stdin prompt, so
retry/resume routing is independent of frame mode and wording. `--shim-frame
off` submits verbatim and skips the issue fetch.

Diagnostics: in the Multica workdir layout (workdir root above the checkout)
the exact submitted prompt is persisted to
`<workdir>/codex-cloud-shim.submitted-prompt.txt` — the cloud task page is
the only other record and it sits behind a browser login. Never written when
the CWD is the checkout itself (it must not be able to enter a diff).

Ownership close: after a successful landing on an ownership turn the shim
runs `multica issue status <id> in_review` — the Ownership-mode workflow
step the local agent would have run (never `done`: review belongs to a
human). Best-effort (a failure is a warn log, never fatal), before the
result event (the daemon may tear the process down once it sees the
result), and never on reply turns (platform rule: Reply mode changes no
status).

Success = exit 0 + stdout's last non-empty line
matching `^https://chatgpt\.com/codex/tasks/(task_[A-Za-z0-9_]+)$`
(probe/exec.out: exactly one line, task id = last path segment, measured
shape `task_e_[0-9a-f]{32}` logged if deviating; stderr empty). Then: write
marker → E1 → E2 → POLL.

Failures:
- stderr matching `environment '.*' not found` → **F4a** fail-fast, verbatim.
- other nonzero / no parsable URL / timeout → **orphan reconcile** (C2): wait
  one poll interval, `list`; candidates = tasks absent from SNAPSHOT with
  `updated_at ≥ submitMark − 2m` (clock skew). Zero → **F4**. One or more →
  **F4b** listing candidate ids — candidates are reported, never adopted: the
  env label (repo slug) is shared by every concurrent run on the repo and
  titles are model-generated, so no candidate can be positively identified as
  this run's submission; adopting a neighbor's task would land a foreign diff
  as ours. Snapshot failed → **F4** (no candidate set to reason over). One
  exec per process, ever; further retries belong to Multica via resume.

### 5.6 POLL

Single loop over a `select`: poll tick (`--shim-poll-interval`), keepalive
tick (`--shim-keepalive`), deadline timer, cancel channel. Driven by the
`Clock` interface.

Each tick: `codex cloud list --env <E> --limit 20 --json` (scratch CWD),
parse per §10 types (probe/list-t010.json), match by id, following `cursor`
up to `--shim-list-pages` pages.

- `pending` → continue (E3 only on status-text change).
- `ready` → E3 with diff stat, break to LAND.
- **any other status** (vocabulary unmeasured — codex-cloud-contract §Open
  items): conservative-verbatim (C4). E3 with the verbatim status; keep
  polling. Same unknown status with `updated_at` unchanged for 5 consecutive
  polls → **F8** (`reached unhandled status "<verbatim>" and stopped
  progressing`). `updated_at` moving → keep waiting until deadline.
  `taxonomy.go` declares `var terminalFailureStatuses = []string{}` —
  deliberately empty, commented with the open item; a captured failure status
  becomes a one-line addition that short-circuits to F8 immediately.
- Id missing from all pages: tolerate 3 consecutive polls (on the 3rd, fall
  back to `codex cloud status <ID>` bracket-parse — probe/status-*.out);
  still nothing → **F7**.
- `list` subprocess/parse failure: transient; 5 consecutive → **F7** with the
  last stderr ≤ 2000 bytes. Counter resets on any success.
- Deadline → **F5**: `deadline 30m0s exceeded; task <ID> last status "<s>"` +
  the standard NOT-cancelled trailer. `session_id` set — the run continues
  upstream and a resume-retry lands the finished work; the deadline never
  orphans the run.

### 5.7 LAND (`status == "ready"`)

Emit E5 first. Landing runs under the same overall deadline as polling (the
flag is the launch → result budget): every landing subprocess inherits a
context bounded at `start + deadline`, so a hung `git push` or wedged apply
dies at the deadline as its stage failure instead of hanging silently. A
landing keepalive ticker keeps the at-least-one-event-per-window guarantee
through the blocking stages (diff and apply carry 300 s budgets each), and
progress `log` events precede diff, apply, and push. Stages compose
(`push ⊃ commit ⊃ apply ⊃ report`):

0. **Idempotency pre-check** (commit/push): `git log -n 50
   --grep="Codex-Cloud-Task: <id>" --format=%H` — hit means an interrupted
   prior run already landed → E7 success `already landed as <sha> (no-op)`.
1. **Empty diff** (list `summary` null or `files_changed==0`): E7 success
   `no changes (empty diff)`, no apply, no commit — any mode.
2. **report**: `codex cloud diff <ID>` (scratch CWD; unified diff, full
   40-hex index lines, exit 0 — probe/diff.patch). Failure → **F9**. Mode
   `report` stops → E7 with the diff embedded (1 MiB truncation).
3. **apply** (`apply|commit|push`): `codex cloud apply <ID> [--attempt N]` in
   the **worktree**, hygiene-wrapped (§3). Measured semantics honored: exit 0
   leaves the change **staged**; re-apply idempotent (exit 0); conflict →
   exit 1, `Apply failed … (applied=0, skipped=1, conflicts=0)`, tree
   untouched (probe/apply-error.log) → **F10** with stderr tail and a note
   that `codex cloud diff <ID>` reproduces the patch. "Tree untouched" is
   claimed only for that measured shape (nonzero exit + parsed counts line);
   any other apply error (timeout kill, hygiene failure) reports the tree
   state as unverified and points at `git status`. After success,
   `git diff --cached --name-status` yields the file list; nothing staged
   (idempotent re-run) → E7 success `patch already present; nothing to
   commit`. Mode `apply` stops → E7 `applied and staged (not committed)`.
4. **commit** (`commit|push`): per §7.3. Failure (missing identity etc.) →
   **F11**, text notes the diff remains applied and staged.
5. **push** (`push` only): `git push -u origin HEAD`. Failure → **F12**, text
   carries the local commit sha so work is not lost.

Then E7 → flush → exit 0.

---

## 6. Failure taxonomy

Exit code: `0` = success result emitted; `1` = error result emitted (E8/E9);
`143` = CANCEL (no result); `1` also if stdout broke and no result could be
written. The *event stream* is the daemon's contract; the exit code matches
claude-CLI convention (C1).

| Code | State | Emission | `session_id` | Fresh-retry phrase | Exit | Notes |
|---|---|---|---|---|---|---|
| F0 `E_CONFIG` | INIT | E8 | omitted | no | 1 | argv/duration parse error |
| F1 `E_INPUT` | READ_PROMPT | E8 | omitted | no | 1 | stdin missing/malformed/oversized/timeout |
| F2 `E_PREFLIGHT` | PREFLIGHT | E8 | omitted | no | 1 | binary absent / `login status` nonzero, verbatim |
| F3 `E_GIT_CONTEXT` | RESOLVE | E8 | omitted | no | 1 | no origin / bad slug / branch chain exhausted |
| F4 `E_SUBMIT` | SUBMIT | E8 | omitted | no | 1 | exec failed, no orphan found; verbatim stderr |
| F4a `E_ENV_NOT_FOUND` | SUBMIT | E8 | omitted | no | 1 | verbatim `environment '<x>' not found` |
| F4b `E_SUBMIT_AMBIGUOUS` | SUBMIT | E8 | omitted | no | 1 | >1 orphan candidate; ids listed, never guessed |
| F13 `E_ISSUE_CONTEXT` | SUBMIT | E8 | omitted | no | 1 | ownership turn whose issue cannot be materialized (no id, or `multica issue get` failed); submitting would be a paid contentless run |
| F5 `E_DEADLINE` | POLL | E8 | task id | no | 1 | run continues upstream; resume re-attaches |
| F7 `E_POLL` | POLL | E8 | task id | no | 1 | 5 consecutive list failures / task vanished (3 misses + status fallback) |
| F8 `E_UPSTREAM_STATUS` | POLL | E8 | task id | no | 1 | unknown status + stalled `updated_at` (5 polls), or a listed terminal status; verbatim |
| F9 `E_DIFF` | LAND | E8 | task id | no | 1 | diff fetch failed |
| F10 `E_APPLY_CONFLICT` | LAND | E8 | task id | no | 1 | tree untouched (measured) |
| F11 `E_COMMIT` | LAND | E8 | task id | no | 1 | diff remains staged |
| F12 `E_PUSH` | LAND | E8 | task id | no | 1 | commit sha reported |
| `R_RESUME_NOT_FOUND` | RESUME_ROUTE | **E9** | omitted | **yes** | 1 | the only reject-phrase emitter; fresh submit known-safe |
| — CANCEL | any | nothing | — | — | 143 | stdin EOF pre-result / SIGTERM; upstream untouched |

Design invariants: `session_id` omitted ⇔ no task exists (nothing for the
daemon to store); `session_id` present on every post-submit failure ⇒ retries
reconcile instead of duplicating; the reject phrase appears in exactly one
template and is scrubbed from all quoted upstream text. The no-result CANCEL
is deliberate: `finalizeStreamResult` falls back to the last assistant text
turn (multica-claude-contract §Output), and every E3/E4 carries the task id +
status, so even a killed shim leaves the id in the transcript and the marker
makes the next run adopt it.

Non-failures worth naming: no diff produced (success); nothing to commit /
already landed (success, idempotent); orphan adopted (log warn, run
proceeds); unknown status while `updated_at` moves (narration until deadline).

---

## 7. Git operations (`gitx`)

All via `exec.Command("git", …)` with explicit `cmd.Dir`, contexts, stderr
captured, errors wrapped `%w` with argv.

### 7.1 Origin → env label

`git remote get-url origin`, matched in order:
`^git@[^:]+:(.+?)(\.git)?$` · `^ssh://[^/]+/(.+?)(\.git)?$` ·
`^https?://[^/]+/(.+?)(\.git)?$`; take the **last two** path segments as
`owner/repo`, strip `.git`. Precedent: the CLI itself parses the SSH origin
to the slug (`env: parsed SSH GitHub origin => mepuka/codex-cloud-shim` —
probe/error.log) and `--env` accepts the slug label, which IS the default env
label (codex-cloud-contract §Environment; `environment_label` in
probe/list-t010.json).

### 7.2 Base branch (always passed — `--branch` otherwise defaults to the submitting CWD's branch, which for a task worktree is an agent branch that may not exist upstream)

1. `--shim-branch` — the explicit pin.
2. `git rev-parse --abbrev-ref --symbolic-full-name @{u}` → strip
   `<remote>/`; if it equals the current branch name the agent branch is
   self-tracking (already pushed) and says nothing about the base → continue.
3. `git symbolic-ref --short refs/remotes/origin/HEAD` → strip `origin/`.
4. `git show-ref --verify refs/remotes/origin/main` → `main`; else `master`.
5. F3 — the shim does not invent a branch name.

The chosen base and deciding rule are emitted as an E6 log event. Rationale:
submit against the base the agent branch tracks (it exists upstream by
construction); the diff applies onto the agent worktree; genuine drift
surfaces as the measured tree-untouched apply failure (F10).

### 7.3 Commit construction

- Issue key: `--shim-issue-key` wins; else first match of
  `\b[A-Z][A-Z0-9]{1,9}-[1-9][0-9]{0,8}\b` in the prompt; else none — never
  guessed (house rule).
- Title: cloud `title` from the last list snapshot; fallback first prompt
  line ≤ 60 runes; ultimate fallback `codex-cloud: <id>`. Append ` (KEY)`
  iff a key exists.
- Message via `git commit -F -`:

```
<title>[ (KEY)]

Codex-Cloud-Task: <task-id>
Codex-Cloud-URL: <url>
Codex-Cloud-Base: <base-branch>
```

- No `-a`, no `git add`: commit exactly what apply staged. Identity comes
  from the worktree's git config (Multica's checkout owns it); a
  missing-identity failure is F11.

### 7.4 error.log hygiene

Per §3 / C7. The `.git/info/exclude` entry is belt-and-braces for the crash
window between apply and restore; it never touches the delivered tree.

---

## 8. Concurrency & shutdown

Three goroutines: main state machine; stdin drainer (EOF → cancel channel);
signal watcher (SIGTERM/SIGINT → same channel). Emission is synchronous under
the emitter mutex — no output goroutine. One `context.Context` cancelled by
the channel flows into every subprocess (`CommandContext`), so CANCEL kills
only local codex children; there is no code path that invokes any upstream
cancel verb (the stub's unknown-verb trap proves none is ever called). Exit
within 2 s of cancel; scratch dir removed best-effort.

---

## 9. `cloud` adapter

`cloud.Client{Bin string, Scratch string}` — exact argv per call:

- `Login(ctx)` → `codex login status`
- `Exec(ctx, env, branch, attempts, prompt)` → `codex cloud exec --env <E>
  --branch <B> [--attempts N] <prompt>` (prompt one argv element)
- `List(ctx, env, cursor)` → `codex cloud list --env <E> --limit 20 --json
  [--cursor C]`
- `Status(ctx, id)` → `codex cloud status <id>` (fallback only; first-line
  bracket token `[PENDING]`/`[READY]` — probe/status-*.out)
- `Diff(ctx, id)` → `codex cloud diff <id>` (report mode + error text only)
- `Apply(ctx, worktree, id, attempt)` → `codex cloud apply <id>
  [--attempt N]`, `Dir=worktree`, hygiene-wrapped

`types.go` (fields exactly as probe/list-t010.json):

```go
type ListResponse struct {
    Tasks  []Task  `json:"tasks"`
    Cursor *string `json:"cursor"`
}
type Task struct {
    ID               string   `json:"id"`
    URL              string   `json:"url"`
    Title            string   `json:"title"`
    Status           string   `json:"status"`
    UpdatedAt        string   `json:"updated_at"`
    EnvironmentID    *string  `json:"environment_id"`    // measured null
    EnvironmentLabel string   `json:"environment_label"`
    Summary          *Summary `json:"summary"`
    IsReview         bool     `json:"is_review"`
    AttemptTotal     int      `json:"attempt_total"`
}
type Summary struct {
    FilesChanged int `json:"files_changed"`
    LinesAdded   int `json:"lines_added"`
    LinesRemoved int `json:"lines_removed"`
}
```

`UpdatedAt` parsed RFC3339Nano on demand (stall detection, orphan window);
parse failure degrades (stall check skipped / candidate ineligible), never a
crash.

---

## 10. Test plan

Gate: `gofmt -l . == ∅`, `go vet ./...`, `go test ./...` — all offline, no
ports, never the real `codex` binary (fixed decision).

### 10.1 Stub codex fixture (`internal/testsupport`)

`WriteStub(t, dir, scenario)` writes an executable shell script `codex`
(0755) into `dir`; tests pass `--shim-codex-bin <dir>/codex`. Data-driven
from a scenario directory (`$CODEX_STUB_DIR` baked into the script):

```
scenario/
  login.out        # default "Logged in using ChatGPT"
  login.exit       # default 0
  exec.out         # default the measured URL line (probe/exec.out)
  exec.exit        # default 0
  exec.sleep       # optional seconds (exec-timeout tests)
  list.1.json …    # served in order via a counter file; last repeats
  list.exit
  status.out       # bracket format (probe/status-*.out)
  status.exit
  diff.patch       # default probe/diff.patch
  apply.exit       # default 0
  apply.stderr     # e.g. measured "Apply failed … (applied=0, skipped=1, conflicts=0)"
  apply.patch      # applied on apply.exit==0
  calls.log        # stub-appended: one line per invocation: CWD + full "$@"
```

Behavior per subcommand:
- **Every `cloud` branch appends a line to `./error.log` in its CWD** —
  reproducing the measured side effect (probe/error.log) so the hygiene
  logic is genuinely exercised.
- `login status` → cat login.out, exit login.exit.
- `cloud exec` → optional sleep, cat exec.out, exit exec.exit.
- `cloud list` → serve `list.<counter>.json`, clamp at last (so
  pending→pending→ready is three files and the third repeats).
- `cloud status` → cat status.out, exit status.exit.
- `cloud diff` → cat diff.patch, exit 0.
- `cloud apply` → exit-0 path runs real `git apply --3way
  "$CODEX_STUB_DIR/apply.patch"` in CWD (staging and idempotency are genuine
  git behavior, matching probe/apply-error.log semantics); exit-1 path prints
  apply.stderr, touches nothing.
- Anything else → exit 64 + calls.log entry (catches drifted invocations and
  proves no cancel verb is ever called).

List fixtures are byte-derived from probe/list-t010.json (pending variant:
`"status":"pending"`, zero/`null` summary).

`GitFixture(t)`: real local repo pair in `t.TempDir()` — bare "origin" + a
worktree clone on `agent/test/task-1` tracking `origin/main`, `origin/HEAD`
set. No network, no ports.

### 10.2 Pure unit tests

- `argv`: full arity-table sweep, both `=`/space forms, interleavings,
  unknown-flag tolerance (no-consume rule), `--resume` capture, every
  `--shim-*` flag, malformed durations → F0, `--version` short-circuit.
- `proto`: golden-byte tests for E1–E9 (exact field order, one line, trailing
  newline); result latch; 1 MiB truncation; `session_id`/`is_error`
  omitempty; no constructor for `user`/`control_request`.
- `prompt`: content array vs bare string, multi-block join, oversize; issue
  key: `DEV-123` mid-prompt, first-of-several, lowercase/`task_e_…`
  non-matches, no-match → empty.
- `gitx` parsers: slug table (scp-like, `ssh://`, https, ±`.git`, extra path
  segments).
- `state`: marker round-trip, corrupt-marker tolerance.
- taxonomy: grep test — `resumeRejectPhrase` appears in exactly one message
  template; the redaction filter replaces phrase substrings in quoted text.

### 10.3 State machine with fakes (`Clock`, in-memory `cloud.Client`/git)

- Keepalive cadence: 7 m of pending → exactly three E4s at 2/4/6 m; any E3
  resets the window.
- Deadline F5; list-failure F7 (5-consecutive, reset on success); vanish
  tolerance + status fallback; unknown-status F8 stall vs moving
  `updated_at`; cursor pagination bound (`--shim-list-pages`).
- CANCEL mid-poll: no result event, no landing, fake records zero cancel
  calls, exit 143.

### 10.4 Integration (stub codex × real git fixture, tiny durations)

Run `run.Run` (plus one compiled-binary test for signals) with piped stdio,
`--shim-poll-interval 20ms --shim-keepalive 50ms --shim-deadline 2s`; assert
the exact ordered event stream, `calls.log`, and repo state:

1. **Happy commit** (default land): events E1,E2,E3…,E5,E7 in order; commit
   titled `<cloud title> (DEV-123)` (key planted in prompt) with all three
   trailers; branch not pushed; worktree `git status` clean; `error.log`
   absent; calls.log shows `--branch main` + `--env <slug>` on exec, every
   CWD outside the worktree except apply.
2. **Land modes**: report (pristine worktree, diff embedded in result), apply
   (staged, no commit), push (bare origin ref advanced).
3. **Empty diff** → success `no changes`, zero apply calls.
4. **Resume adopt**: `--resume` + matching marker, list has it pending → zero
   exec calls, polls to ready, lands. Ready-at-once variant lands directly.
5. **Resume follow-up**: hash differs → one exec whose prompt argv starts
   with `Follow-up to Codex Cloud task`; push-mode variant with pre-pushed
   agent branch asserts `--branch agent/test/task-1`.
6. **Resume not found** (marker hash matches, list empty, status fails) → E9
   golden bytes, zero exec calls.
7. **Orphan adoption**: exec.exit=1, list.2 grows one unseen task → adopted,
   log warn, exactly one exec in calls.log. Two unseen tasks → F4b with both
   ids.
8. **Reconcile**: seed a marker for the same prompt hash + alive task → fresh
   run adopts, zero exec calls. Dead-task variant deletes marker and submits.
9. **Env not found**: exec stderr fixture → F4a verbatim; no marker written.
10. **Unknown status**: `"status":"weird_state"` with frozen `updated_at` →
    F8 after 5 polls, verbatim status, session_id set. Moving-`updated_at`
    variant runs to deadline → F5.
11. **Apply conflict**: apply.exit=1 with measured stderr → F10, worktree
    hash-identical before/after, error.log still scrubbed.
12. **error.log hygiene**: tracked-error.log repo variant → post-land
    `git status` clean, content = index content; untracked-preexisting
    variant → truncated to prior size.
13. **Idempotent re-land**: run twice over the same worktree → second run
    reports `already landed`, one commit total.
14. **Fast exit** (compiled binary): SIGTERM and stdin-close mid-poll →
    process exits 143 within 2 s, stdout ends without a `result` line, no
    post-close stub invocations, no cancel verb ever in calls.log.
15. **Poll resilience**: 4 list failures then success → completes; 5 → F7.
16. **Phrase guard**: stub stderr seeded with the reject phrase → E8 output
    contains `[redacted-resume-phrase]`, never the phrase. Shared checker
    over every scenario's stdout: line-JSON throughout; first event `system`
    with non-empty `session_id` whenever a task exists; zero
    `user`/`control_request` events; `async_launched` never appears; exactly
    one `result`, final; `session_id` constant across a run.

---

## 11. Pre-implementation verifications (open — never guessed in code)

- **V1 — reject-phrase list**: read `resumeRejectedPhrases` at
  `multica@8df7549 server/pkg/agent/claude.go:769`, pin the exact strings in
  `taxonomy.go` (E9 uses one verbatim; the phrase-guard test covers all).
- **V2 — failure-status vocabulary**: unmeasured (codex-cloud-contract §Open
  items). The empty `terminalFailureStatuses` + verbatim narration + stall
  rule + deadline backstop are correct without it; a captured failure status
  is a one-line addition.
- **V3 — `--attempts > 1` shapes**: `--shim-attempts`/`--shim-attempt` pass
  through untouched; `attempt_total` is surfaced in the report; nothing else
  interprets attempts.
- **V4 — ambiguous env label**: `--shim-env` is the pin; failures surface
  verbatim as F4a/F4.
