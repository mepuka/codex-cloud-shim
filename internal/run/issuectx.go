package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Ownership-mode prompts carry no task content at all — measured against
// multica @ 37f3bb7 (server/internal/daemon/prompt.go, buildPromptBody): the
// assignment prompt names the issue id and tells the agent to fetch the rest
// with `multica issue get`. A local tool-using agent does exactly that; the
// cloud model cannot run the CLI, so the second live run
// (task_e_6a70e5f1d6e08323a58e27380c69be64, 2026-08-03) spent its 43 s
// "checking out main" and landed an empty diff even with the frame in place.
// The shim therefore materializes the issue itself at submit time — the same
// mechanical-convention move as bootstrapCheckout — and embeds it in the
// framed prompt. Reply-mode prompts embed their trigger comment inline
// (buildCommentPrompt), so they degrade usefully without the fetch;
// ownership turns do not, and failing beats paying for a no-op run.

const (
	taskContextRelPath = ".multica/daemon_task_context.json"
	issueFetchTimeout  = 60 * time.Second
	// ownershipMarker is the daemon's stable mode line (prompt.go
	// turnModeOwnership); its presence means the prompt body carries no task
	// content beyond the issue pointer.
	ownershipMarker = "Turn mode: Ownership."
)

// daemonTaskContext mirrors the execenv sidecar the daemon writes at the
// workdir root (measured 2026-08-03: managed_by/agent_id/issue_id).
type daemonTaskContext struct {
	IssueID string `json:"issue_id"`
}

// issueIDLine is the assignment line every daemon-built prompt carries
// (prompt.go: `Your assigned issue ID is: %s`); the fallback when the
// sidecar is absent.
var issueIDLine = regexp.MustCompile(`Your assigned issue ID is: ([0-9a-fA-F-]{36})`)

// issueFields is the subset of `multica issue get --output json` the prompt
// needs; unknown fields are ignored.
type issueFields struct {
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// issueID resolves the dispatching issue: the machine-managed sidecar first
// (workdir root, or one level up when the CWD was already the checkout),
// the prompt's assignment line as fallback. "" = not a Multica-dispatched
// run; nothing to fetch.
func (r *runner) issueID() string {
	for _, root := range []string{r.workdirRoot, filepath.Dir(r.workdirRoot)} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(taskContextRelPath)))
		if err != nil {
			continue
		}
		var tc daemonTaskContext
		if json.Unmarshal(b, &tc) == nil && tc.IssueID != "" {
			return tc.IssueID
		}
	}
	if m := issueIDLine.FindStringSubmatch(r.prompt); m != nil {
		return m[1]
	}
	return ""
}

// issueContext fetches the issue and renders the task block for the cloud
// prompt. Failure policy: an ownership turn whose issue cannot be fetched
// would submit a contentless prompt — a paid upstream no-op — so it fails
// (F13); every other shape degrades with a warning, because the raw prompt
// either carries its trigger comment or is not a Multica dispatch at all.
func (r *runner) issueContext(ctx context.Context) (string, *failure) {
	if r.s.frame == frameOff {
		return "", nil
	}
	id := r.issueID()
	ownership := strings.Contains(r.prompt, ownershipMarker)
	if id == "" {
		if ownership {
			return "", failf(codeIssueContext, "",
				"ownership turn but no issue id found (no %s sidecar, no assignment line in the prompt); a contentless cloud submit would be a paid no-op", taskContextRelPath)
		}
		return "", nil
	}

	fctx, cancel := context.WithTimeout(ctx, issueFetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(fctx, r.multicaBin(), "issue", "get", id, "--output", "json")
	cmd.Dir = r.workdirRoot
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	var iss issueFields
	if err == nil {
		err = json.Unmarshal(out, &iss)
	}
	if err == nil && iss.Title == "" && iss.Description == "" {
		err = fmt.Errorf("issue %s parsed but carries no title or description", id)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", nil // CANCEL surfaces in the caller
		}
		if ownership {
			return "", failf(codeIssueContext, "",
				"multica issue get %s failed (%v); refusing to submit a contentless ownership prompt to the cloud", id, err)
		}
		r.log("warn", fmt.Sprintf("issue context unavailable (%v); submitting with the dispatch prompt only", err))
		return "", nil
	}

	var b strings.Builder
	b.WriteString("The dispatched work item")
	if iss.Identifier != "" {
		fmt.Fprintf(&b, " (issue %s)", iss.Identifier)
	}
	b.WriteString(":\n\n")
	if iss.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n\n", iss.Title)
	}
	if iss.Description != "" {
		b.WriteString(iss.Description)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
