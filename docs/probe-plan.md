# Probe plan — pinning the Codex Cloud CLI contract

The shim's other half is written against measured behavior of
`codex-cli 0.146.0`, not its docs. One paid run against a scratch environment
pins everything below. Environment: create at chatgpt.com/codex settings for
`mepuka/codex-cloud-shim`, defaults are fine; paste the ENV_ID here when known.

## Facts already pinned (free, from `--help` on 0.146.0)

- `codex cloud exec --env <ENV_ID> [--branch B] [--attempts N] [QUERY]` —
  `--branch` "defaults to current branch" (the submitting CWD's branch — the
  shim must ALWAYS pass it explicitly).
- `codex cloud list [--env ID] [--limit 1-20] [--cursor C] --json` — measured
  empty shape: `{"tasks": [], "cursor": null}`.
- `codex cloud status <TASK_ID>` — no `--json` flag observed in help.
- `codex cloud diff <TASK_ID>`, `codex cloud apply <TASK_ID> [--attempt N]`.
- Auth: `codex login status` → "Logged in using ChatGPT".

## Questions the probe answers

1. **exec blocking semantics** — does `exec` return the task id immediately,
   or block streaming until completion? What lands on stdout vs stderr, and
   in what format? (Decides the shim's inner loop: wait-on-child vs poll.)
2. **Task id shape** — where does the id appear and what does it look like?
3. **status output** — plain-text states, machine-parseable? Does it carry
   the assistant's message text (→ richer progress narration)?
4. **list --json task schema** — fields per task once non-empty (id, env,
   state, branch, turn ids, PR refs?).
5. **diff/apply behavior** — diff format; apply onto a branch that has
   drifted from the run's base; apply exit codes; `--attempt` numbering.
6. **Timing** — submit→running latency, run duration for a trivial task
   (calibrates poll interval and watchdog margins).
7. **Environment stickiness** — is env id stable across runs; does `list`
   show env metadata usable for `env_label` display.

## Probe protocol

```sh
S=~/probe; mkdir -p $S
# 1. fire, capturing streams separately with timing
( time codex cloud exec --env "$ENV_ID" --branch main \
    "Create docs/PROBE.md containing the current UTC date and one sentence describing this repository. Change nothing else." \
    >$S/exec.out 2>$S/exec.err ) 2>$S/exec.time
# 2. immediately and until terminal state, every 30s:
codex cloud list --env "$ENV_ID" --limit 5 --json >$S/list-$(date +%s).json
codex cloud status "$TASK_ID" >$S/status-$(date +%s).out 2>&1
# 3. terminal:
codex cloud diff "$TASK_ID" >$S/diff.patch 2>$S/diff.err
# 4. apply into a scratch worktree of this repo; record exit code + git status
# 5. re-run apply a second time (idempotency), and once on a dirtied tree
```

Artifacts are committed under `docs/probe/` and become the measured contract
(`docs/codex-cloud-contract.md`), which the implementation cites by capture
file, never by memory.
