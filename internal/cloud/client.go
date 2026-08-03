// Package cloud wraps the codex CLI's cloud subcommands (exec, list, status,
// diff, apply) exactly as measured in docs/codex-cloud-contract.md and
// docs/probe/. The binary path is configurable so tests drive the stub at
// testdata/fake-codex/codex — the real CLI is never invoked in tests.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mepuka/codex-cloud-shim/internal/gitctx"
)

// Default subprocess budgets (design.md §3). exec was measured at 3.4 s
// (docs/probe/exec.time); the budget leaves generous headroom.
const (
	defaultExecTimeout  = 120 * time.Second
	defaultQueryTimeout = 60 * time.Second
	defaultApplyTimeout = 300 * time.Second
	waitDelay           = 5 * time.Second
	listLimit           = 20
	stderrQuoteLimit    = 2000
)

var (
	// ErrEnvNotFound wraps the measured fail-fast
	// `Error: environment '<x>' not found` (F4a).
	ErrEnvNotFound = errors.New("codex cloud environment not found")
	// ErrNoTaskURL means exec exited 0 but its stdout carried no task URL.
	ErrNoTaskURL = errors.New("exec output carried no task URL")
	// ErrTaskNotFound means the id was absent from every list page searched.
	ErrTaskNotFound = errors.New("task not found in cloud list")
	// ErrApplyFailed wraps a nonzero `codex cloud apply` (F10); the measured
	// conflict leaves the tree untouched (docs/probe/apply-error.log).
	ErrApplyFailed = errors.New("codex cloud apply failed")
)

var (
	// Exactly one stdout line, the task URL; id = last path segment
	// (docs/probe/exec.out).
	taskURLRe     = regexp.MustCompile(`^https://chatgpt\.com/codex/tasks/(task_[A-Za-z0-9_]+)$`)
	envNotFoundRe = regexp.MustCompile(`environment '[^']*' not found`)
	// Counts line, both measured forms: the raw capture logs the
	// space-separated `applied=0 skipped=1 conflicts=0`
	// (docs/probe/apply-error.log); the contract prose records the
	// comma-separated terminal form `(applied=0, skipped=1, conflicts=0)`
	// (docs/codex-cloud-contract.md). No raw capture of apply's terminal
	// stdout/stderr pins one (docs/probe-plan.md, open items), so accept
	// both.
	applyCountsRe = regexp.MustCompile(`applied=([0-9]+),? skipped=([0-9]+),? conflicts=([0-9]+)`)
	// `status` first line: `[PENDING] <title>` (docs/probe/status-*.out).
	statusLineRe = regexp.MustCompile(`^\[([A-Z_]+)\][ \t]*(.*)$`)
)

// Client is the codex subprocess adapter. Every invocation runs with CWD =
// Scratch — quarantining the CLI's measured error.log CWD side effect
// (docs/probe/error.log) — except Apply, which must run in the worktree
// because it executes `git apply --3way` in CWD and is hygiene-wrapped
// instead (design.md §3, C7).
type Client struct {
	// Bin is the codex executable; empty means "codex". The test seam.
	Bin string
	// Scratch is the CWD for every call except Apply. Required. The run
	// layer mirrors the worktree's git origin into it (gitctx.MirrorOrigin)
	// so the CLI's measured env-resolution read of the CWD origin
	// (docs/probe/error.log) sees the same context it was probed with.
	Scratch string
	// Env entries are appended to the inherited environment (the stub's
	// scenario switch in tests).
	Env []string

	// Zero values take the measured-informed defaults above.
	ExecTimeout  time.Duration // cloud exec
	QueryTimeout time.Duration // list / status / login
	ApplyTimeout time.Duration // diff / apply
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "codex"
	}
	return c.Bin
}

func (c *Client) execTimeout() time.Duration  { return orDefault(c.ExecTimeout, defaultExecTimeout) }
func (c *Client) queryTimeout() time.Duration { return orDefault(c.QueryTimeout, defaultQueryTimeout) }
func (c *Client) applyTimeout() time.Duration { return orDefault(c.ApplyTimeout, defaultApplyTimeout) }

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// run executes the codex binary with dir as CWD (Scratch when dir is empty).
// No shell anywhere; the prompt travels as one argv element.
func (c *Client) run(ctx context.Context, dir string, timeout time.Duration, args ...string) (stdout, stderr []byte, err error) {
	if dir == "" {
		dir = c.Scratch
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, c.bin(), args...)
	cmd.Dir = dir
	cmd.WaitDelay = waitDelay
	// The codex CLI can spawn its own children (node shims, git for apply);
	// killing only the leader would orphan them holding our pipes. On Unix the
	// whole process group dies with the context; on Windows the default
	// leader-kill stands (no pgid semantics).
	setProcGroup(cmd)
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	if runErr != nil {
		runErr = fmt.Errorf("%s %s: %w", c.bin(), argvSummary(args), runErr)
	}
	return outBuf.Bytes(), errBuf.Bytes(), runErr
}

// argvSummary keeps error text bounded — the exec prompt may be huge.
func argvSummary(args []string) string {
	s := strings.Join(args, " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func truncateBytes(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…[truncated]"
	}
	return string(b)
}

// Submission is a successfully submitted cloud task.
type Submission struct {
	ID  string // session_id for the whole run (fixed decision)
	URL string
}

// LoginStatus preflights auth: `codex login status`, exit code only —
// no stdout phrase-match (design.md C10). The returned text is stdout alone,
// truncated to the standard quote limit so a misbehaving wrapper cannot
// inflate the failure event; the error already carries the stderr quote, so
// stderr is never duplicated into the caller's text.
func (c *Client) LoginStatus(ctx context.Context) (string, error) {
	stdout, stderr, err := c.run(ctx, "", c.queryTimeout(), "login", "status")
	out := strings.TrimSpace(truncateBytes(stdout, stderrQuoteLimit))
	if err != nil {
		return out, fmt.Errorf("codex login status: %w: %s", err, truncateBytes(stderr, stderrQuoteLimit))
	}
	return out, nil
}

// Submit fires `codex cloud exec --env <E> --branch <B> [--attempts N]
// <prompt>` — fire-and-forget, measured 3.4 s (docs/probe/exec.time) — and
// parses the task id from the URL line on stdout. --branch is ALWAYS passed
// (fixed decision).
func (c *Client) Submit(ctx context.Context, env, branch string, attempts int, prompt string) (*Submission, error) {
	args := []string{"cloud", "exec", "--env", env, "--branch", branch}
	if attempts > 0 {
		args = append(args, "--attempts", strconv.Itoa(attempts))
	}
	args = append(args, prompt)
	stdout, stderr, err := c.run(ctx, "", c.execTimeout(), args...)
	if m := envNotFoundRe.Find(stderr); m != nil {
		// fail fast, verbatim (F4a)
		return nil, fmt.Errorf("%w: %s", ErrEnvNotFound, strings.TrimSpace(string(stderr)))
	}
	if err != nil {
		return nil, fmt.Errorf("cloud exec: %w (stderr: %s)", err, truncateBytes(stderr, stderrQuoteLimit))
	}
	line := lastNonEmptyLine(stdout)
	m := taskURLRe.FindStringSubmatch(line)
	if m == nil {
		return nil, fmt.Errorf("%w: last stdout line %q", ErrNoTaskURL, line)
	}
	return &Submission{ID: m[1], URL: line}, nil
}

func lastNonEmptyLine(b []byte) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// List runs one page of `codex cloud list --env <E> --limit 20 --json
// [--cursor C]`.
func (c *Client) List(ctx context.Context, env, cursor string) (*ListResponse, error) {
	args := []string{"cloud", "list", "--env", env, "--limit", strconv.Itoa(listLimit), "--json"}
	if cursor != "" {
		args = append(args, "--cursor", cursor)
	}
	stdout, stderr, err := c.run(ctx, "", c.queryTimeout(), args...)
	if err != nil {
		return nil, fmt.Errorf("cloud list: %w (stderr: %s)", err, truncateBytes(stderr, stderrQuoteLimit))
	}
	var resp ListResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("cloud list: unmarshal: %w", err)
	}
	return &resp, nil
}

// PollResult is one observation of the task's cloud state.
type PollResult struct {
	Task Task
	// State is the conservative mapping; StateUnknown carries the verbatim
	// Task.Status for the caller to narrate, never to interpret.
	State State
}

// Poll finds the task by id in `list --json`, following the cursor up to
// maxPages pages (≥1). The id aging off all pages is ErrTaskNotFound —
// callers tolerate transient misses and fall back to Status (design.md §5.6).
func (c *Client) Poll(ctx context.Context, env, id string, maxPages int) (*PollResult, error) {
	if maxPages <= 0 {
		maxPages = 1
	}
	cursor := ""
	for page := 0; page < maxPages; page++ {
		resp, err := c.List(ctx, env, cursor)
		if err != nil {
			return nil, err
		}
		for _, task := range resp.Tasks {
			if task.ID == id {
				return &PollResult{Task: task, State: MapState(task.Status)}, nil
			}
		}
		if resp.Cursor == nil || *resp.Cursor == "" {
			break
		}
		cursor = *resp.Cursor
	}
	return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
}

// StatusResult is the bracket-parsed first line of `codex cloud status`.
type StatusResult struct {
	State     State
	RawStatus string // lowercased bracket token, verbatim otherwise
	Title     string
}

// Status is the fallback when the id has aged off the list pages: parse the
// human-oriented first line `[PENDING] <title>` (docs/probe/status-*.out).
func (c *Client) Status(ctx context.Context, id string) (*StatusResult, error) {
	stdout, stderr, err := c.run(ctx, "", c.queryTimeout(), "cloud", "status", id)
	if err != nil {
		return nil, fmt.Errorf("cloud status: %w (stderr: %s)", err, truncateBytes(stderr, stderrQuoteLimit))
	}
	line := firstNonEmptyLine(stdout)
	m := statusLineRe.FindStringSubmatch(line)
	if m == nil {
		return nil, fmt.Errorf("cloud status: unparsable first line %q", line)
	}
	raw := strings.ToLower(m[1])
	return &StatusResult{State: MapState(raw), RawStatus: raw, Title: strings.TrimSpace(m[2])}, nil
}

func firstNonEmptyLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(l); s != "" {
			return s
		}
	}
	return ""
}

// Diff fetches the task's unified diff (full 40-hex index lines, exit 0 —
// docs/probe/diff.patch). Truncation for report embedding is the caller's
// concern.
func (c *Client) Diff(ctx context.Context, id string) (string, error) {
	stdout, stderr, err := c.run(ctx, "", c.applyTimeout(), "cloud", "diff", id)
	if err != nil {
		return "", fmt.Errorf("cloud diff: %w (stderr: %s)", err, truncateBytes(stderr, stderrQuoteLimit))
	}
	return string(stdout), nil
}

// ApplyResult reports what `codex cloud apply` did.
type ApplyResult struct {
	// Counts parsed from the measured `(applied=N, skipped=N, conflicts=N)`
	// line when present (docs/probe/apply-error.log).
	Applied   int
	Skipped   int
	Conflicts int
	HasCounts bool
	Stderr    string // ≤2000 bytes, for failure narration
}

// Apply runs `codex cloud apply <id> [--attempt N]` in the worktree — the one
// codex call that cannot be scratch-quarantined — wrapped in error.log
// hygiene (design.md §3/C7): capture state before, idempotent
// .git/info/exclude entry, restore after, success or failure (the measured
// failed apply wrote error.log too). Measured semantics: exit 0 leaves the
// change staged; re-apply idempotent; conflict exits 1 with the tree
// untouched.
func (c *Client) Apply(ctx context.Context, worktree, id string, attempt int) (*ApplyResult, error) {
	pre := captureErrorLog(ctx, worktree)
	args := []string{"cloud", "apply", id}
	if attempt > 0 {
		args = append(args, "--attempt", strconv.Itoa(attempt))
	}
	stdout, stderr, runErr := c.run(ctx, worktree, c.applyTimeout(), args...)
	hygErr := pre.restore(ctx)

	res := &ApplyResult{Stderr: truncateBytes(stderr, stderrQuoteLimit)}
	// Streams are searched separately: gluing them (append) would both alias
	// stdout's backing array and juxtapose the last stdout token with the
	// first stderr token, letting a counts-like fragment spanning the seam
	// parse as corrupt numbers.
	m := applyCountsRe.FindSubmatch(stdout)
	if m == nil {
		m = applyCountsRe.FindSubmatch(stderr)
	}
	if m != nil {
		res.Applied, _ = strconv.Atoi(string(m[1]))
		res.Skipped, _ = strconv.Atoi(string(m[2]))
		res.Conflicts, _ = strconv.Atoi(string(m[3]))
		res.HasCounts = true
	}
	if runErr != nil {
		err := fmt.Errorf("%w: %s (stderr: %s)", ErrApplyFailed, runErr, res.Stderr)
		if hygErr != nil {
			err = errors.Join(err, fmt.Errorf("error.log hygiene: %w", hygErr))
		}
		return res, err
	}
	if hygErr != nil {
		return res, fmt.Errorf("apply succeeded but error.log hygiene failed: %w", hygErr)
	}
	return res, nil
}

// errorLogState captures <worktree>/error.log before an apply so the CLI's
// measured CWD side effect is never delivered (fixed decision; design.md C7).
type errorLogState struct {
	worktree string
	existed  bool
	size     int64
	tracked  bool
}

func captureErrorLog(ctx context.Context, worktree string) errorLogState {
	st := errorLogState{worktree: worktree}
	if fi, err := os.Stat(filepath.Join(worktree, "error.log")); err == nil {
		st.existed = true
		st.size = fi.Size()
	}
	st.tracked = gitQuiet(ctx, worktree, "ls-files", "--error-unmatch", "error.log") == nil
	// Belt-and-braces for the crash window between apply and restore. Never
	// .gitignore: the delivered tree must not change.
	ensureExcludeEntry(ctx, worktree)
	return st
}

// restore runs after apply, success or failure: absent-before → remove;
// untracked-present → truncate to prior size; tracked → git checkout.
func (st errorLogState) restore(ctx context.Context) error {
	p := filepath.Join(st.worktree, "error.log")
	switch {
	case st.tracked:
		if err := gitQuiet(ctx, st.worktree, "checkout", "--", "error.log"); err != nil {
			return fmt.Errorf("restore tracked error.log: %w", err)
		}
	case !st.existed:
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove error.log: %w", err)
		}
	default:
		if err := os.Truncate(p, st.size); err != nil {
			return fmt.Errorf("truncate error.log: %w", err)
		}
	}
	return nil
}

// ensureExcludeEntry idempotently appends "error.log" to .git/info/exclude.
// Best-effort: failure only weakens the crash-window guard.
func ensureExcludeEntry(ctx context.Context, worktree string) {
	gitDir, err := gitOutput(ctx, worktree, "rev-parse", "--git-dir")
	if err != nil {
		return
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktree, gitDir)
	}
	exclude := filepath.Join(gitDir, "info", "exclude")
	if b, err := os.ReadFile(exclude); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(l) == "error.log" {
				return
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, "error.log")
}

func gitQuiet(ctx context.Context, dir string, args ...string) error {
	_, err := gitctx.Run(ctx, dir, args...)
	return err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	return gitctx.Run(ctx, dir, args...)
}
