// Package gitctx reads the git context the shim submits from — origin slug,
// current branch, upstream base — and lands into: commit with issue key,
// own-branch push (design.md §7).
package gitctx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// runWaitDelay bounds cmd.Wait after the context kills git: a git subcommand
// can spawn children that inherit its pipes (`git push` spawns ssh, `git
// commit` runs hooks), and without WaitDelay a killed git whose pipe a child
// still holds blocks Wait forever — a permanent hang in exactly the
// deadline-cancel path the kill was for.
const runWaitDelay = 5 * time.Second

// issueKeyRe is the tighter form from design.md §7.3 (C8): a key found in the
// prompt is used verbatim, never guessed.
var issueKeyRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-[1-9][0-9]{0,8}\b`)

// IssueKey returns the first issue key (e.g. DEV-123) in text, or "".
func IssueKey(text string) string {
	return issueKeyRe.FindString(text)
}

// ParseSlug maps a git remote URL to its owner/repo slug — the default Codex
// Cloud environment label (docs/codex-cloud-contract.md "Environment
// resolution"; the CLI itself parses the SSH origin the same way,
// docs/probe/error.log "env: parsed SSH GitHub origin"). Handles scp-like
// (git@host:o/r.git), ssh://, and http(s):// forms; takes the last two path
// segments and strips .git (design.md §7.1).
func ParseSlug(remoteURL string) (string, error) {
	u := strings.TrimSpace(remoteURL)
	if u == "" {
		return "", errors.New("empty remote URL")
	}
	var path string
	switch {
	case strings.Contains(u, "://"):
		// ssh://user@host/o/r.git, https://host/o/r.git, git://host/o/r
		rest := u[strings.Index(u, "://")+3:]
		i := strings.Index(rest, "/")
		if i < 0 {
			return "", fmt.Errorf("remote URL %q has no path", remoteURL)
		}
		path = rest[i+1:]
	case strings.Contains(u, ":"):
		// scp-like: user@host:o/r.git
		path = u[strings.Index(u, ":")+1:]
	default:
		return "", fmt.Errorf("unrecognized remote URL form %q", remoteURL)
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	segs := strings.Split(path, "/")
	if len(segs) < 2 || segs[len(segs)-1] == "" || segs[len(segs)-2] == "" {
		return "", fmt.Errorf("remote URL %q: cannot derive owner/repo slug", remoteURL)
	}
	return segs[len(segs)-2] + "/" + segs[len(segs)-1], nil
}

// MirrorOrigin makes dir a minimal git repository carrying the same origin
// remote URL as src — no objects copied, no network. The measured record
// shows the codex CLI consulting its CWD's git origin during env resolution
// (docs/probe/error.log "env: parsed SSH GitHub origin"), and every probe
// capture was taken from inside a checkout; mirroring the origin keeps the
// scratch-CWD quarantine (the CLI's error.log side effect lands in dir)
// while preserving the env-resolution context the CLI demonstrably reads.
func MirrorOrigin(ctx context.Context, src, dir string) error {
	url, err := git(ctx, src, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("read origin of %s: %w", src, err)
	}
	if _, err := git(ctx, dir, "init", "--quiet"); err != nil {
		return fmt.Errorf("init scratch repo: %w", err)
	}
	if _, err := git(ctx, dir, "remote", "add", "origin", url); err != nil {
		return fmt.Errorf("add scratch origin: %w", err)
	}
	return nil
}

// DiscoverWorktree resolves the actual repo checkout from a launch CWD.
// Multica launches agents with CWD = the task workdir, whose root carries the
// generated brief while the repository checkout sits ONE LEVEL BELOW (measured
// against a live task workdir 2026-08-03; re-measured by this shim's first
// in-platform run failing E_GIT_CONTEXT at the workdir root). Rule: the CWD
// itself if it is a repo; else exactly ONE immediate child that is a repo;
// zero or several candidates is an error naming them — never a guess, because
// picking the wrong checkout would submit a different repo's env and land a
// diff in the wrong tree.
func DiscoverWorktree(ctx context.Context, cwd string) (dir, rule string, err error) {
	if _, gitErr := Run(ctx, cwd, "rev-parse", "--git-dir"); gitErr == nil {
		return cwd, "cwd is a repo", nil
	}
	entries, readErr := os.ReadDir(cwd)
	if readErr != nil {
		return "", "", fmt.Errorf("cwd is not a git repository and cannot be scanned: %w", readErr)
	}
	var candidates []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// A checkout carries .git as a dir (clone) or a file (linked
		// worktree); a stat covers both and avoids a git exec per child.
		if _, statErr := os.Stat(filepath.Join(cwd, e.Name(), ".git")); statErr == nil {
			candidates = append(candidates, e.Name())
		}
	}
	switch len(candidates) {
	case 1:
		return filepath.Join(cwd, candidates[0]), "single checkout under the task workdir", nil
	case 0:
		return "", "", errors.New("cwd is not a git repository and no checkout was found one level below; launch in the checkout or fix the workdir")
	default:
		return "", "", fmt.Errorf("cwd is not a git repository and %d checkouts sit below it (%s); ambiguous — refusing to guess",
			len(candidates), strings.Join(candidates, ", "))
	}
}

// OriginSlug resolves the repo's origin remote to its owner/repo slug.
func OriginSlug(ctx context.Context, dir string) (string, error) {
	url, err := git(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("no origin remote: %w", err)
	}
	return ParseSlug(url)
}

// CurrentBranch returns the checked-out branch name; "HEAD" means detached.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	return git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// BaseBranch detects the base branch the cloud task should target, per the
// design.md §7.2 chain. The returned rule names the deciding step for log
// narration. The shim never invents a branch name: chain exhausted = error
// (F3) with an instruction to pin --shim-branch.
func BaseBranch(ctx context.Context, dir string) (branch, rule string, err error) {
	cur, err := CurrentBranch(ctx, dir)
	if err != nil {
		return "", "", err
	}
	// 1. upstream of HEAD — unless self-tracking (an already-pushed agent
	// branch tracks itself and says nothing about the base).
	if up, upErr := git(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); upErr == nil {
		if i := strings.Index(up, "/"); i >= 0 {
			if base := up[i+1:]; base != cur {
				return base, "upstream of HEAD", nil
			}
		}
	}
	// 2. origin/HEAD
	if ref, refErr := git(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); refErr == nil {
		return strings.TrimPrefix(ref, "origin/"), "origin/HEAD", nil
	}
	// 3. origin/main, then origin/master
	for _, b := range []string{"main", "master"} {
		if _, refErr := git(ctx, dir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+b); refErr == nil {
			return b, "origin/" + b + " exists", nil
		}
	}
	return "", "", errors.New("base branch undetectable (no upstream, no origin/HEAD, no origin/main|master); pin --shim-branch via custom_args")
}

// CommitInput is everything a landing commit carries (design.md §7.3).
type CommitInput struct {
	Title    string // cloud title or first-prompt-line fallback
	IssueKey string // appended as " (KEY)" when non-empty; never guessed
	TaskID   string // Codex-Cloud-Task trailer; also the ultimate title fallback
	TaskURL  string // Codex-Cloud-URL trailer
	Base     string // Codex-Cloud-Base trailer
}

// Message renders the exact commit message: title line plus the durable
// trailers that survive the merge (the branch name dies at cleanup; the
// commit title and trailers are the code → commit → task chain).
func (in CommitInput) Message() (string, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" && in.TaskID != "" {
		title = "codex-cloud: " + in.TaskID
	}
	if title == "" {
		return "", errors.New("empty commit title and no task id fallback")
	}
	if in.IssueKey != "" {
		title += " (" + in.IssueKey + ")"
	}
	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n")
	trailers := [][2]string{
		{"Codex-Cloud-Task", in.TaskID},
		{"Codex-Cloud-URL", in.TaskURL},
		{"Codex-Cloud-Base", in.Base},
	}
	wroteBlank := false
	for _, tr := range trailers {
		if tr[1] == "" {
			continue
		}
		if !wroteBlank {
			b.WriteString("\n")
			wroteBlank = true
		}
		fmt.Fprintf(&b, "%s: %s\n", tr[0], tr[1])
	}
	return b.String(), nil
}

// CommitStaged commits exactly what is already staged — never `git add`,
// never `-a` (design.md §7.3: apply stages only patch paths; this keeps
// error.log out of the commit even if hygiene restore races). Identity comes
// from the worktree's git config; a missing-identity failure surfaces
// verbatim (F11). Returns the new commit sha.
func CommitStaged(ctx context.Context, dir string, in CommitInput) (string, error) {
	msg, err := in.Message()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "commit", "-F", "-")
	cmd.Dir = dir
	cmd.WaitDelay = runWaitDelay // commit runs hooks; see runWaitDelay
	cmd.Stdin = strings.NewReader(msg)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git commit: %w (%s)", err,
			strings.TrimSpace(outBuf.String()+errBuf.String()))
	}
	return git(ctx, dir, "rev-parse", "HEAD")
}

// Push pushes the current branch only — `git push -u origin HEAD`, the
// platform's delivery path and the single push form the house rules allow
// (AGENTS.md envelope-B exception). Detached HEAD refuses: HEAD would not
// name an own branch.
func Push(ctx context.Context, dir string) error {
	cur, err := CurrentBranch(ctx, dir)
	if err != nil {
		return err
	}
	if cur == "HEAD" {
		return errors.New("refusing to push: detached HEAD is not an own branch")
	}
	if _, err := git(ctx, dir, "push", "-u", "origin", "HEAD"); err != nil {
		return fmt.Errorf("push own branch %s: %w", cur, err)
	}
	return nil
}

// Run executes one git command with dir as CWD and returns trimmed stdout;
// stderr is captured into the error. The single git-exec path for every
// package in this module: context-killable, WaitDelay-bounded (see
// runWaitDelay). Exported so cloud, run and state share it instead of
// growing divergent copies.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.WaitDelay = runWaitDelay
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err,
			strings.TrimSpace(errBuf.String()))
	}
	return strings.TrimSpace(outBuf.String()), nil
}

// git is the package-internal alias for Run.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	return Run(ctx, dir, args...)
}
