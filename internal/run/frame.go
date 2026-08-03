package run

import "fmt"

// Prompt frame modes (--shim-frame). Default cloud: the first live in-Multica
// run (DEV-87 follow-up, 2026-08-03) submitted the platform's brief verbatim
// and produced an empty diff — the brief addresses a local tool-using agent
// (checkout commands, threaded replies, issue-status moves), and the cloud
// model answered it conversationally instead of editing files. The frame
// re-addresses the same brief to the agent that actually runs it.
const (
	frameCloud = "cloud"
	frameOff   = "off"
)

// framedPrompt wraps the submit prompt with the cloud-facing preamble. It is
// applied at SUBMIT only — marker identity (state.PromptHash) stays on the
// raw stdin prompt, so reconcile and resume routing are unaffected by frame
// mode or future wording changes.
func (r *runner) framedPrompt(p string) string {
	if r.s.frame == frameOff {
		return p
	}
	return fmt.Sprintf(`You are Codex working in your own cloud workspace, on a checkout of %s (base branch %s).

The task below may be written for a local agent on the machine that dispatched it. Any instructions in it about local platform mechanics — checking out repositories, registering or calling local CLIs, posting comments or replies, moving issue status — do not apply to you; other machinery handles them. Do not reply that you cannot do those things.

Your deliverable is the code change itself. Edit the files in your workspace until the work described below is done; a prose answer with no file changes is a failed run. Your changes are collected as a diff and landed by the dispatcher.

--- task ---

%s`, r.env, r.branch, p)
}
