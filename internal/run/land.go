package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/mepuka/codex-cloud-shim/internal/cloud"
	"github.com/mepuka/codex-cloud-shim/internal/gitctx"
	"github.com/mepuka/codex-cloud-shim/internal/protocol"
)

// land runs the §5.7 landing stages for a ready task and returns the E7
// result text. Stages compose: push ⊃ commit ⊃ apply ⊃ report.
func (r *runner) land(ctx context.Context, task *cloud.Task) (string, *failure) {
	mode := r.s.landMode
	r.say(fmt.Sprintf("Task ready; landing with mode %s (%s).", mode, landPipeline(mode)))

	// 0. Idempotency pre-check (commit/push): an interrupted prior run may
	// already have landed this task (C9).
	if mode == landCommit || mode == landPush {
		if sha, ok := alreadyLanded(ctx, r.cfg.Worktree, r.sid); ok {
			return r.successText(task, fmt.Sprintf("already landed as %s (no-op)", sha), "", ""), nil
		}
	}

	// 1. Empty diff: success in any mode, no apply, no commit. When the
	// ready observation came from the status fallback (no list summary),
	// emptiness is decided by fetching the diff itself.
	if task.Summary != nil {
		if task.Summary.FilesChanged == 0 {
			return r.successText(task, "no changes (empty diff)", "", ""), nil
		}
	} else if mode != landReport {
		r.log("info", "fetching diff to verify the task produced changes")
		d, err := r.client.Diff(ctx, r.sid)
		if err != nil {
			return "", failf(codeDiff, r.sid, "diff fetch failed: %v", err)
		}
		if strings.TrimSpace(d) == "" {
			return r.successText(task, "no changes (empty diff)", "", ""), nil
		}
	}

	// 2. report: fetch and embed the diff — stats alone would defeat the
	// mode's purpose (C6).
	if mode == landReport {
		r.log("info", "fetching diff for the report")
		d, err := r.client.Diff(ctx, r.sid)
		if err != nil {
			return "", failf(codeDiff, r.sid, "diff fetch failed: %v", err)
		}
		if strings.TrimSpace(d) == "" {
			return r.successText(task, "no changes (empty diff)", "", ""), nil
		}
		embed := "\nDiff:\n```diff\n" + protocol.Truncate(d, protocol.MaxEmbeddedBytes) + "\n```"
		return r.successText(task, "report only (no worktree changes)", "", embed), nil
	}

	// 3. apply (apply|commit|push): worktree CWD, hygiene-wrapped by the
	// cloud client (§3/C7). Measured semantics: exit 0 leaves the change
	// staged; conflict exits 1 with the tree untouched.
	r.log("info", "applying diff to worktree")
	if res, err := r.client.Apply(ctx, r.cfg.Worktree, r.sid, r.s.attempt); err != nil {
		if errors.Is(err, cloud.ErrApplyFailed) && res != nil && res.HasCounts {
			// The measured conflict shape — nonzero exit plus the counts
			// line — leaves the tree untouched (docs/probe/apply-error.log).
			return "", failf(codeApplyConflict, r.sid,
				"apply failed (tree untouched): %v\n`codex cloud diff %s` reproduces the patch", err, r.sid)
		}
		// Any other apply error — a timeout kill mid `git apply --3way`,
		// or apply-succeeded-but-hygiene-failed — carries no measured
		// guarantee about the tree.
		return "", failf(codeApplyConflict, r.sid,
			"apply failed and the worktree state is unverified — inspect `git status` before retrying: %v\n`codex cloud diff %s` reproduces the patch", err, r.sid)
	}
	files, err := stagedNameStatus(ctx, r.cfg.Worktree)
	if err != nil {
		return "", failf(codeApplyConflict, r.sid, "apply succeeded but staged files could not be listed: %v", err)
	}
	if files == "" {
		// Idempotent re-run over an already-applied tree.
		return r.successText(task, "patch already present; nothing to commit", "", ""), nil
	}
	if mode == landApply {
		return r.successText(task, "applied and staged (not committed)", files, ""), nil
	}

	// 4. commit (commit|push): title from the cloud task, issue key from the
	// flag or the prompt — never guessed (§7.3).
	key := r.s.issueKey
	if key == "" {
		key = gitctx.IssueKey(r.prompt)
	}
	in := gitctx.CommitInput{
		Title:    r.commitTitle(task),
		IssueKey: key,
		TaskID:   r.sid,
		TaskURL:  r.url,
		Base:     r.branch,
	}
	sha, err := gitctx.CommitStaged(ctx, r.cfg.Worktree, in)
	if err != nil {
		return "", failf(codeCommit, r.sid, "commit failed (the diff remains applied and staged): %v", err)
	}
	branch, _ := gitctx.CurrentBranch(ctx, r.cfg.Worktree)
	title, _ := in.Message()
	title = strings.SplitN(title, "\n", 2)[0]
	landed := fmt.Sprintf("commit %s on %s — %q", shortSHA(sha), branch, title)

	// 5. push (push only): the own-branch delivery path.
	if mode == landPush {
		r.log("info", "pushing agent branch to origin")
		if err := gitctx.Push(ctx, r.cfg.Worktree); err != nil {
			return "", failf(codePush, r.sid,
				"push failed (work is preserved as local commit %s): %v", sha, err)
		}
		landed += "\nPushed: origin/" + branch
	}
	return r.successText(task, landed, files, ""), nil
}

// successText renders E7 (design.md §4.2).
func (r *runner) successText(task *cloud.Task, landed, files, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Codex Cloud task %s completed.", r.sid)
	if t := taskTitle(task); t != "" {
		fmt.Fprintf(&b, "\nTitle: %s", t)
	}
	fmt.Fprintf(&b, "\nURL: %s", r.url)
	if task != nil && task.Summary != nil && task.Summary.FilesChanged > 0 {
		fmt.Fprintf(&b, "\nDiff: +%d/-%d across %d %s",
			task.Summary.LinesAdded, task.Summary.LinesRemoved,
			task.Summary.FilesChanged, plural(task.Summary.FilesChanged, "file"))
	}
	fmt.Fprintf(&b, "\nLanded: %s", landed)
	if files != "" {
		fmt.Fprintf(&b, "\nFiles:\n%s", files)
	}
	b.WriteString(extra)
	return b.String()
}

// commitTitle: cloud title, else first prompt line ≤ 60 runes, else the
// gitctx fallback `codex-cloud: <id>` (C8).
func (r *runner) commitTitle(task *cloud.Task) string {
	if t := taskTitle(task); t != "" {
		return t
	}
	line := strings.SplitN(strings.TrimSpace(r.prompt), "\n", 2)[0]
	runes := []rune(line)
	if len(runes) > 60 {
		line = string(runes[:60])
	}
	return line
}

func taskTitle(task *cloud.Task) string {
	if task == nil {
		return ""
	}
	return strings.TrimSpace(task.Title)
}

func landPipeline(mode string) string {
	switch mode {
	case landReport:
		return "diff only"
	case landApply:
		return "apply"
	case landPush:
		return "apply → commit → push"
	default:
		return "apply → commit"
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// alreadyLanded greps recent history for the task trailer (C9).
func alreadyLanded(ctx context.Context, dir, taskID string) (string, bool) {
	out, err := gitOut(ctx, dir, "log", "-n", "50", "--grep=Codex-Cloud-Task: "+taskID, "--format=%H")
	if err != nil || out == "" {
		return "", false
	}
	return shortSHA(strings.SplitN(out, "\n", 2)[0]), true
}

// stagedNameStatus lists what apply staged; empty means nothing staged.
func stagedNameStatus(ctx context.Context, dir string) (string, error) {
	return gitOut(ctx, dir, "diff", "--cached", "--name-status")
}

// remoteHasBranch reports whether origin already carries branch (the
// follow-up-in-push-mode base rule, §5.4).
func remoteHasBranch(ctx context.Context, dir, branch string) bool {
	out, err := gitOut(ctx, dir, "ls-remote", "--heads", "origin", branch)
	return err == nil && out != ""
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
