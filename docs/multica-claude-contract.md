# The Multica-side contract (verified)

What Multica's claude backend actually sends to and parses from the command a
`protocol_family: claude` runtime profile launches. Every fact below was read
from `multica @ 8df7549`, `server/pkg/agent/claude.go` unless noted. This is
the contract the shim must satisfy; the Codex-Cloud-side contract lives in
`probe-plan.md` until the probe pins it.

## Invocation

- The daemon resolves the profile's `command_name` on PATH (per-machine
  override via `multica runtime profile set-path`), registers the runtime only
  on hosts where it resolves, and probes `<cmd> --version` (best-effort; empty
  version is acceptable) — `server/internal/daemon/daemon.go:1719-1821`.
- Launch (`buildClaudeArgs`, claude.go:668):

  ```
  <cmd> -p --output-format stream-json --input-format stream-json --verbose \
        --permission-mode bypassPermissions --disallowedTools AskUserQuestion \
        [--strict-mcp-config] [--model M] [--effort E] [--max-turns N] \
        [--resume SESSION_ID] [extra_args...] [custom_args...] [--settings PATH]
  ```

  The shim must accept and may ignore all of these. Per-agent `custom_args`
  pass through `filterCustomArgs` — this is the shim's configuration channel
  (e.g. `--attempts 3`, `--land pr|apply`).
- CWD = the task worktree (a real checkout of the target repo on the task's
  agent branch). `--mcp-config <path>` may be injected (claude.go:65).
- stdin receives exactly one JSON line (`buildClaudeInput`, claude.go:739):

  ```json
  {"type":"user","message":{"role":"user","content":[{"type":"text","text":"<prompt>"}]}}
  ```

  stdin stays open after that (control-response channel); the daemon closes it
  when it sees the `result` event, on cancellation, and at stream end.

## Output: JSONL on stdout, one event per line, max 10 MB/line

Envelope (`claudeSDKMessage`): `type`, `session_id`, `message` (raw), plus
result fields. Event types the daemon consumes (event loop, claude.go:176-303):

| type | daemon behavior | shim obligation |
|---|---|---|
| `system` | captures `session_id`, emits "running" status | REQUIRED early: `{"type":"system","session_id":"<cloud-task-id>"}` |
| `assistant` | parses `message` as `{role, model, content:[blocks], usage}`; `text` blocks stream + accumulate as fallback output; `thinking`, `tool_use` forwarded; per-model token usage summed | OPTIONAL — progress narration; also feeds the inactivity watchdog |
| `user` | forwards `tool_result` blocks; **any `status:"async_launched"` inside fails the whole run** ("Multica-managed runs require foreground execution") | never emit |
| `result` | terminal: `result` text, `is_error`, `session_id`, `usage`/`modelUsage`; daemon closes stdin | REQUIRED: exactly one, last |
| `log` | `{log:{level,message}}` forwarded | optional |
| `control_request` | daemon auto-allows and writes a `control_response` line to stdin | never emit; never read stdin after the prompt except to drain |

If no `result` event arrives, `finalizeStreamResult` falls back to the last
assistant `text`-only turn — but the shim should always emit `result`.

Usage is optional: it is only accumulated when `message.usage` is present with
a non-empty `message.model`. Cloud runs may report none; that is well-formed.

## Session identity and resume

- The shim's `session_id` IS the Codex Cloud task id. The daemon stores the
  `result`-reported session id and passes it back verbatim as `--resume <id>`.
- Resume rejection is phrase-matched (`resumeRejectedPhrases`, claude.go:769)
  against failure output/stderr; a positive match makes the daemon drop the
  session pointer and retry fresh. Deliberate design lever: the shim can emit
  a matching phrase to request a fresh-session retry, and must NOT emit one
  incidentally.

## Liveness

- `ExecOptions` carries `SemanticInactivityTimeout` and `IdleWatchdogTimeout`
  (daemon-side no-message watchdogs). Claude-family defaults were not pinned
  in this pass — treat "emit an event at least every few minutes" as the rule;
  periodic poll-status assistant messages satisfy it.
- On cancel/timeout the daemon closes stdin, SIGTERMs the process group, then
  SIGKILLs survivors (claude.go:192-213). The shim must exit promptly on
  stdin-close/SIGTERM and must NOT kill the upstream cloud run (the durable
  record is the point: the id is already in the transcript).

## Delivery path (design intent, not backend contract)

The worktree CWD means the shim can: resolve repo → ENV_ID from
`git remote get-url origin` + its config map; pick the base branch from the
agent branch's upstream; on completion `codex cloud apply <id> [--attempt N]`
into the worktree, commit (issue key in title when present), and
`git push -u origin HEAD` — the platform's normal delivery path. Alternatively
`--land pr` leaves landing to Codex Cloud's native PR creation
(`TaskResponse.external_pull_requests`, verified against openai/codex@main).
