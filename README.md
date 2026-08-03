# codex-cloud-shim

Codex Cloud as a Claude Code-compatible CLI.

A stream-json shim: any harness that can drive the `claude` CLI in
non-interactive mode (`-p --output-format stream-json`) can use this binary to
fire, track, and land [Codex Cloud](https://chatgpt.com/codex) tasks instead —
cloud compute, local delivery.

Built for (but not limited to) [Multica](https://multica.ai) custom runtime
profiles: register this command as a workspace runtime profile with
`protocol_family: claude` and Codex Cloud becomes a first-class runtime.

Status: contract-pinning phase. Design docs land in `docs/`.
