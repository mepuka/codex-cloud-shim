package run

import "fmt"

// resumeRejectPhrase must literally match an entry in Multica's
// resumeRejectedPhrases (server/pkg/agent/claude.go:769 @ 8df7549;
// docs/multica-claude-contract.md §Session identity). It appears in exactly
// one template — the E9 resume-not-found result — and is scrubbed from every
// other event via protocol.RedactPhrases so an incidental upstream match can
// never fire the daemon's fresh-retry lever (design.md §4.1).
const resumeRejectPhrase = "No conversation found with session ID"

// terminalFailureStatuses is deliberately empty: the cloud failure-status
// vocabulary is unmeasured (docs/codex-cloud-contract.md §Open items,
// design.md C4/V2). Unknown statuses are narrated verbatim and only the
// stall rule (unchanged updated_at for stallLimit polls) or the deadline
// terminates them. A captured failure status becomes a one-line addition
// here and short-circuits to F8 immediately.
var terminalFailureStatuses = []string{}

// Failure codes (design.md §6). The code token leads the result text's first
// line; session_id presence follows the taxonomy table: omitted ⇔ no task
// exists, set on every post-submit failure so retries reconcile.
const (
	codeConfig         = "E_CONFIG"           // F0
	codeInput          = "E_INPUT"            // F1
	codePreflight      = "E_PREFLIGHT"        // F2
	codeGitContext     = "E_GIT_CONTEXT"      // F3
	codeSubmit         = "E_SUBMIT"           // F4
	codeEnvNotFound    = "E_ENV_NOT_FOUND"    // F4a
	codeSubmitAmbig    = "E_SUBMIT_AMBIGUOUS" // F4b
	codeDeadline       = "E_DEADLINE"         // F5
	codePoll           = "E_POLL"             // F7
	codeUpstreamStatus = "E_UPSTREAM_STATUS"  // F8
	codeDiff           = "E_DIFF"             // F9
	codeApplyConflict  = "E_APPLY_CONFLICT"   // F10
	codeCommit         = "E_COMMIT"           // F11
	codePush           = "E_PUSH"             // F12
	codeIssueContext   = "E_ISSUE_CONTEXT"    // F13
)

// failure is one taxonomy row on its way to an E8/E9 result event.
type failure struct {
	code string
	sid  string // "" = omit session_id (pre-submit / E9)
	msg  string
	// resumeReject marks E9, the only event allowed to carry the reject
	// phrase; it is never redacted and never carries a session_id (the
	// pointer is being dropped).
	resumeReject bool
}

// failf builds an E8-bound failure; the code token leads the first line.
func failf(code, sid, format string, a ...any) *failure {
	return &failure{code: code, sid: sid, msg: code + ": " + fmt.Sprintf(format, a...)}
}

// resumeNotFound builds the E9 failure (design.md §4.2 E9): the deliberate
// lever that makes the daemon drop the session pointer and retry fresh.
func resumeNotFound(id string) *failure {
	return &failure{
		code:         "R_RESUME_NOT_FOUND",
		resumeReject: true,
		msg: fmt.Sprintf("%s: %s. The Codex Cloud task id could not be found upstream; start a fresh session.",
			resumeRejectPhrase, id),
	}
}
