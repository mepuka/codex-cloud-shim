# Changelog

## v0.1.0 (2026-08-03)

codex-cloud-shim is a Claude Code-compatible CLI that fronts Codex Cloud, providing a streamlined workflow to submit tasks, poll for their completion, and land the resulting changes locally.

- Claude Code-compatible `stream-json` protocol
- Submit-then-poll task execution
- Landing modes: report, apply, commit, and push
- Reconcile marker for safe result recovery
- `error.log` hygiene
- Process-group containment
