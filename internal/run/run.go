// Package run is the shim's lifecycle state machine (design.md §5): read the
// prompt, preflight, resolve env/branch, route resume/reconcile, submit,
// poll with keepalive and deadline, land, emit exactly one result. Nothing
// imports run except cmd/codex-cloud-shim.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mepuka/codex-cloud-shim/internal/cloud"
	"github.com/mepuka/codex-cloud-shim/internal/envcheck"
	"github.com/mepuka/codex-cloud-shim/internal/gitctx"
	"github.com/mepuka/codex-cloud-shim/internal/protocol"
	"github.com/mepuka/codex-cloud-shim/internal/state"
)

// Exit codes (design.md §6, C1): 0 = success result, 1 = error result,
// 143 = CANCEL (no result; upstream run untouched).
const (
	ExitSuccess = 0
	ExitError   = 1
	ExitCancel  = 143
)

const (
	promptReadTimeout = 60 * time.Second // design.md §5.1
	stallLimit        = 5                // unknown status, frozen updated_at (C4)
	missLimit         = 3                // id absent from list pages (§5.6)
	listFailLimit     = 5                // consecutive list failures (§5.6)
	orphanSkew        = 2 * time.Minute  // orphan-adoption clock skew (§5.5)
)

// Config wires the runner to the process (cmd/codex-cloud-shim owns the
// os.* edges; tests inject pipes and a fixture worktree).
type Config struct {
	Args     *protocol.ParsedArgs
	Stdin    io.Reader
	Emitter  *protocol.Emitter
	Worktree string
	Getenv   func(string) string
}

type runner struct {
	cfg    Config
	s      *settings
	em     *protocol.Emitter
	client *cloud.Client
	// cancelRun aborts the lifecycle from inside: a failed event WRITE means
	// the daemon (the only stdout reader) is gone, and continuing would poll
	// a paid run for up to the full deadline with nobody listening.
	cancelRun context.CancelFunc

	start    time.Time
	lastEmit time.Time

	sid    string // the cloud task id = session_id (fixed decision)
	url    string
	env    string
	branch string
	prompt string
	// workdirRoot is the CWD as launched — the daemon's task workdir, where
	// the .multica sidecars live — captured before ensureWorktree narrows
	// cfg.Worktree to the checkout.
	workdirRoot string
	// issueRef is the dispatching issue id once issueContext resolved it;
	// closeOwnership reuses it instead of re-reading the sidecar.
	issueRef string
	// worktreeSlug is the checkout's origin identity at resolve time; the
	// landing path re-derives and compares it (identity guard, see resolve).
	worktreeSlug string

	// lastTask is the freshest list observation — the commit-title source.
	lastTask   *cloud.Task
	lastStatus string
	adoptNote  string // E2 lead-in: Submitted / Adopted existing / Resuming
}

// Run executes the full lifecycle and returns the process exit code. ctx
// cancellation is the CANCEL path: SIGTERM/SIGINT (wired by main) or stdin
// EOF before the result (wired here); no result is emitted, the upstream
// cloud run is never touched, local codex children die with the context.
func Run(ctx context.Context, cfg Config) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	r := &runner{cfg: cfg, em: cfg.Emitter, start: time.Now(), cancelRun: cancel}

	s, err := newSettings(cfg.Args, cfg.Getenv)
	if err != nil {
		return r.fail(failf(codeConfig, "", "%v", err)) // F0
	}
	r.s = s

	for _, w := range cfg.Args.Warnings {
		r.log("warn", w)
	}

	r.workdirRoot = cfg.Worktree

	// WORKTREE: discovery (CWD, or one checkout below — the Multica workdir
	// layout), falling back once to the platform's own convention of running
	// `multica repo checkout` from the workdir's resources sidecar (measured:
	// the daemon never pre-clones; real agents run the checkout themselves).
	// Everything downstream — env slug, branch, apply, commit, push, the
	// reconcile marker — keys off the resolved checkout.
	if f := r.ensureWorktree(ctx); f != nil {
		return r.fail(f)
	}

	// A hard kill (Multica's SIGKILL escalation, Windows termination, the
	// 2 s exit backstop) skips the deferred RemoveAll, so scratch dirs can
	// accumulate across runs; sweep stale ones from prior processes first.
	sweepStaleScratch(os.TempDir(), time.Now())

	scratch, err := os.MkdirTemp("", scratchPrefix+"*")
	if err != nil {
		return r.fail(failf(codeConfig, "", "create scratch dir: %v", err))
	}
	defer os.RemoveAll(scratch)
	// The scratch quarantine keeps the CLI's error.log CWD side effect out
	// of the worktree (§3/C7), but every probe capture was taken from
	// inside a checkout and the raw record shows the CLI reading the CWD's
	// git origin during env resolution (docs/probe/error.log "env: parsed
	// SSH GitHub origin"). Mirror the worktree's origin into the scratch
	// dir so codex sees the same env-resolution context there; a bare
	// non-repo CWD is unmeasured (docs/probe-plan.md, open items).
	if err := gitctx.MirrorOrigin(ctx, r.cfg.Worktree, scratch); err != nil {
		r.log("warn", fmt.Sprintf(
			"scratch dir lacks git origin context (%v); codex env resolution from a non-repo CWD is unmeasured", err))
	}
	r.client = &cloud.Client{Bin: s.codexBin, Scratch: scratch, ExecTimeout: s.execTimeout}

	// READ_PROMPT (F1) with the 60 s guard; the daemon writes the line
	// immediately after launch (claude.go:739).
	prompt, f := readPromptGuarded(ctx, cfg.Stdin)
	if ctx.Err() != nil {
		return ExitCancel
	}
	if f != nil {
		return r.fail(f)
	}
	r.prompt = prompt

	// Drainer: nothing meaningful arrives after the prompt (the shim never
	// emits control_request); EOF before the result is the daemon's cancel
	// (claude.go:192-213). EOF after the result is the normal close.
	go func() {
		io.Copy(io.Discard, cfg.Stdin)
		if !r.em.ResultEmitted() {
			cancel()
		}
	}()

	code := r.run(ctx)
	if ctx.Err() != nil && !r.em.ResultEmitted() {
		return ExitCancel
	}
	return code
}

// run is the post-stdin state machine; every subprocess call carries ctx.
func (r *runner) run(ctx context.Context) int {
	// PREFLIGHT (F2)
	codexPath, err := exec.LookPath(r.s.codexBin)
	if err != nil {
		return r.failOrCancel(ctx, failf(codePreflight, "", "codex binary %q not found: %v", r.s.codexBin, err))
	}
	// A .cmd/.bat shim (npm's codex.cmd) is executed through cmd.exe, whose
	// command line terminates at the first newline — the multi-line prompt
	// silently truncates to line 1 and every cloud task returns an empty
	// diff titled after that line. Measured 2026-08-03: five live runs
	// truncated this way while the same prompt via the native exe landed
	// +12/-0. Refuse loudly; there is no safe way to pass newlines to a
	// batch shim.
	if ext := strings.ToLower(filepath.Ext(codexPath)); ext == ".cmd" || ext == ".bat" {
		return r.failOrCancel(ctx, failf(codePreflight, "",
			"codex resolved to a batch shim (%s); cmd.exe truncates multi-line prompts at the first newline, so cloud submissions would silently lose the task body. Point --shim-codex-bin (or CODEX_CLOUD_SHIM_CODEX_BIN) at the native executable, e.g. the npm package's vendor codex.exe", codexPath))
	}
	if out, err := r.client.LoginStatus(ctx); err != nil {
		return r.failOrCancel(ctx, failf(codePreflight, "", "codex login status failed: %v\n%s", err, out))
	}

	// RESOLVE (F3)
	if f := r.resolve(ctx); f != nil {
		return r.failOrCancel(ctx, f)
	}

	// RESUME_ROUTE / RECONCILE / SNAPSHOT+SUBMIT
	if f := r.route(ctx); f != nil {
		return r.failOrCancel(ctx, f)
	}
	if ctx.Err() != nil {
		// Cancelled mid-submit: submit/route return a nil failure on
		// cancellation, and E1/E2 must not be emitted after cancel — an
		// empty-sid system event could be captured by the daemon during
		// its own teardown (§5.1).
		return ExitCancel
	}

	// E1 + E2 — the daemon captures session_id from E1 and flips to running.
	r.noteEmit(r.em.SystemInit(r.sid))
	r.say(fmt.Sprintf("%s Codex Cloud task %s (env %s, base branch %s).\n%s\nPolling every %s; deadline %s.",
		r.adoptNote, r.sid, r.env, r.branch, r.url,
		fmtDur(r.s.pollInterval), fmtDur(r.s.deadline)))

	// POLL
	task, f := r.poll(ctx)
	if f != nil || ctx.Err() != nil {
		return r.failOrCancel(ctx, f)
	}

	// LAND — under the same overall wall-clock budget as polling (the
	// deadline flag is launch → result, design.md §2.2): every landing
	// subprocess (diff, apply, git commit, git push) inherits landCtx, so
	// a hung push dies at the deadline as its stage failure (F9–F12)
	// instead of hanging the shim silently forever. The keepalive
	// guarantee is kept by a landing ticker — diff and apply carry 300 s
	// budgets each, longer than the keepalive window.
	landCtx, cancelLand := context.WithDeadline(ctx, r.start.Add(r.s.deadline))
	defer cancelLand()
	stopKeepalive := r.startLandingKeepalive(ctx)
	text, f := r.land(landCtx, task)
	stopKeepalive()
	if f != nil || ctx.Err() != nil {
		return r.failOrCancel(ctx, f)
	}
	// Ownership close BEFORE the result: the daemon may tear the process
	// down once it sees the result event.
	r.closeOwnership(ctx)
	// A result that could not be written is exit 1 (design.md §6): on a
	// broken stdout (daemon died, EPIPE) the shim must not report success.
	if err := r.em.ResultSuccess(r.sid, time.Since(r.start).Milliseconds(), text); err != nil {
		return ExitError
	}
	return ExitSuccess
}

// startLandingKeepalive keeps the at-least-one-event-per-window guarantee
// alive through the landing stages, where blocking subprocess calls can be
// silent for longer than the keepalive window (docs/multica-claude-contract.md
// §Liveness: the daemon runs no-message inactivity watchdogs). The ticker
// emits directly on the emitter (its own mutex serializes; the result latch
// makes a late tick a refused no-op) and never touches lastEmit, which
// belongs to the main goroutine. The returned stop func is idempotent.
func (r *runner) startLandingKeepalive(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(r.s.keepalive)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				r.em.Assistant(r.sid, fmt.Sprintf(
					"Still landing Codex Cloud task %s; elapsed %s.",
					r.sid, fmtDur(time.Since(r.start))))
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// resolve pins env and base branch, narrating both decisions (design.md §5.2).
//
// Identity guard: the origin slug is the one identity shared by the checkout
// (where the diff lands) and the environment label (what the cloud task runs
// against). Every path that could split those two identities — a pinned env
// over a foreign checkout, a swapped checkout mid-run, a stale marker — is
// checked against the slug, because landing repo X's diff into checkout Y is
// the failure that actually hurts. No cryptographic binding is possible or
// pretended: everything here runs as one OS user, and the daemon-created
// workdir is the trust anchor.
func (r *runner) resolve(ctx context.Context) *failure {
	slug, slugErr := gitctx.OriginSlug(ctx, r.cfg.Worktree)
	if slugErr == nil {
		r.worktreeSlug = slug
	}

	envRule := "--shim-env"
	r.env = r.s.env
	if r.env == "" {
		if slugErr != nil {
			return failf(codeGitContext, "", "cannot derive env from origin (%v); pin --shim-env via custom_args", slugErr)
		}
		r.env = slug
		envRule = "origin slug"
	} else if slugComparable(r.env) && slugErr == nil && !strings.EqualFold(r.env, slug) {
		return failf(codeGitContext, "",
			"identity mismatch: --shim-env pins %q but the checkout's origin is %q — refusing to submit against one repo and land into another",
			r.env, slug)
	}
	r.log("info", fmt.Sprintf("env %s (rule: %s)", r.env, envRule))

	branchRule := "--shim-branch"
	r.branch = r.s.branch
	if r.branch == "" {
		base, rule, err := gitctx.BaseBranch(ctx, r.cfg.Worktree)
		if err != nil {
			return failf(codeGitContext, "", "%v", err)
		}
		r.branch, branchRule = base, rule
	}
	r.log("info", fmt.Sprintf("base branch %s (rule: %s)", r.branch, branchRule))
	return nil
}

// route decides which cloud task this run is about: resume retry/follow-up
// (§5.4), marker reconcile (§5.3), or a fresh submit (§5.5). On return the
// runner carries sid, url and adoptNote.
//
// Reconcile-by-list depends entirely on the marker surviving: with no marker
// (lost or corrupt) a re-launched identical prompt submits fresh — cloud
// titles are model-generated, so there is no marker-free identity to hunt an
// existing duplicate by.
func (r *runner) route(ctx context.Context) *failure {
	marker, mErr := state.Load(ctx, r.cfg.Worktree)
	if mErr != nil {
		r.log("warn", fmt.Sprintf("ignoring unreadable marker: %v", mErr))
		marker = nil
	}
	hash := state.PromptHash(r.prompt)

	if r.s.resume != "" {
		if marker != nil && marker.PromptSHA256 == hash && marker.TaskID == r.s.resume {
			// Retry of the same prompt: adopt if alive, E9 if gone —
			// list just proved the task dead, so the daemon's fresh
			// retry is known-safe (§5.4).
			task, found, f := r.aliveness(ctx, r.s.resume)
			if f != nil {
				return f
			}
			if !found {
				return resumeNotFound(r.s.resume)
			}
			return r.adopt(ctx, task, "Resuming", hash)
		}
		// Follow-up: a NEW task; the old id is named in the prompt only
		// when a marker existed to name it (§5.4).
		submitPrompt := r.prompt
		if marker != nil {
			submitPrompt = fmt.Sprintf(
				"Follow-up to Codex Cloud task %s; earlier changes for this task may already exist on the branch.\n\n%s",
				marker.TaskID, r.prompt)
		}
		if r.s.landMode == landPush && r.s.branch == "" {
			// In push mode the delivered agent branch may already be
			// upstream; the follow-up builds on it (§5.4).
			if cur, err := gitctx.CurrentBranch(ctx, r.cfg.Worktree); err == nil && cur != "HEAD" &&
				remoteHasBranch(ctx, r.cfg.Worktree, cur) {
				r.branch = cur
				r.log("info", fmt.Sprintf("base branch %s (rule: follow-up on pushed agent branch)", cur))
			}
		}
		return r.submit(ctx, submitPrompt, hash)
	}

	if marker != nil && marker.Env != "" && r.worktreeSlug != "" &&
		slugComparable(marker.Env) && !strings.EqualFold(marker.Env, r.worktreeSlug) {
		// Identity guard: the marker was written beside a checkout whose
		// origin no longer matches — adopting its task would attach this
		// run to another repo's submission.
		return failf(codeGitContext, "",
			"identity mismatch: reconcile marker records env %q but the checkout's origin is %q — refusing to adopt across repos; delete %s/codex-cloud-shim/state.json under the git dir if the origin change was deliberate",
			marker.Env, r.worktreeSlug, "<git-dir>")
	}

	if marker != nil && marker.PromptSHA256 == hash {
		// RECONCILE: same prompt re-launched without --resume — a retry
		// whose session pointer was lost. Never double-submit (fixed
		// decision).
		task, found, f := r.aliveness(ctx, marker.TaskID)
		if f != nil {
			return f
		}
		if found {
			return r.adopt(ctx, task, "Adopted existing", hash)
		}
		r.log("warn", fmt.Sprintf("marker task %s no longer exists upstream; submitting fresh", marker.TaskID))
		if err := state.Delete(ctx, r.cfg.Worktree); err != nil {
			r.log("warn", fmt.Sprintf("delete stale marker: %v", err))
		}
	}
	return r.submit(ctx, r.prompt, hash)
}

// aliveness checks whether a task id still exists upstream: list pages
// first, `codex cloud status` bracket-parse when aged off (§5.3). A
// transport failure is a poll failure (E8 with the id) — never a silent
// duplicate submit and never a spurious E9. found=false is reserved for a
// positively-identified not-found response; nothing in docs/probe/ captures
// what `codex cloud status <dead-id>` returns (docs/probe-plan.md, open
// items), and Status also errors on timeouts and unparsable banner lines, so
// today every status error is conservatively a poll failure and the
// E9/fresh-submit branches stay dormant until that shape is measured.
func (r *runner) aliveness(ctx context.Context, id string) (*cloud.Task, bool, *failure) {
	res, err := r.client.Poll(ctx, r.env, id, r.s.listPages)
	if err == nil {
		return &res.Task, true, nil
	}
	if !errors.Is(err, cloud.ErrTaskNotFound) {
		return nil, false, failf(codePoll, id, "aliveness check failed: %v", err)
	}
	st, sErr := r.client.Status(ctx, id)
	if sErr != nil {
		// Indistinguishable from "gone": a spurious E9 (daemon drops the
		// session pointer) or a fresh submit here would duplicate a
		// still-live run — the exact outcome the fixed decision ("on
		// retry reconcile via list before submitting a duplicate")
		// forbids. Fail as F7 so the retry reconciles by id instead.
		return nil, false, failf(codePoll, id,
			"task %s absent from %d list page(s) and the status fallback failed: %v",
			id, r.s.listPages, sErr)
	}
	return &cloud.Task{ID: id, URL: taskURL(id), Title: st.Title, Status: st.RawStatus}, true, nil
}

// adopt attaches to an existing task: refresh the marker, set identity, and
// let poll's immediate first tick short-circuit ready to LAND.
func (r *runner) adopt(ctx context.Context, task *cloud.Task, note, hash string) *failure {
	r.sid = task.ID
	r.url = task.URL
	if r.url == "" {
		r.url = taskURL(task.ID)
	}
	r.lastTask = task
	r.adoptNote = note
	r.saveMarker(ctx, hash)
	return nil
}

// submit is SNAPSHOT + SUBMIT + orphan reconcile (§5.5). One exec per
// process, ever; further retries belong to Multica via resume.
func (r *runner) submit(ctx context.Context, submitPrompt, hash string) *failure {
	seen := map[string]bool{}
	snapshotOK := false
	submitMark := time.Now()
	if tasks, err := r.listAll(ctx); err != nil {
		r.log("warn", fmt.Sprintf("pre-submit snapshot failed (orphan reconcile disabled): %v", err))
	} else {
		snapshotOK = true
		for _, t := range tasks {
			seen[t.ID] = true
		}
	}

	// Frame + issue context are applied here and nowhere else: hash (marker
	// identity) was computed on the raw prompt by the caller, so
	// reconcile/resume routing never sees either.
	issueBlock, f := r.issueContext(ctx)
	if f != nil {
		return f
	}
	if ctx.Err() != nil {
		return nil // CANCEL surfaces in the caller
	}
	framed := r.framedPrompt(issueBlock, submitPrompt)
	r.captureSubmittedPrompt(framed)
	sub, err := r.client.Submit(ctx, r.env, r.branch, r.s.attempts, framed)
	if err != nil {
		if ctx.Err() != nil {
			return nil // CANCEL surfaces in the caller
		}
		if errors.Is(err, cloud.ErrEnvNotFound) {
			// F4a: upstream text verbatim, plus the one fix that exists —
			// creation has no API, so point at the web UI and the check
			// command instead of leaving a mystery failure.
			return failf(codeEnvNotFound, "",
				"%v\nNo Codex Cloud environment is attached to this repo. Create one (defaults are fine) at %s, then verify with `codex-cloud-shim env check %s`. This is a one-time step per repo.",
				err, envcheck.CreateURL, r.env)
		}
		// Orphan reconcile: the exec may have died after submitting. No
		// candidate can be positively identified as ours — the env label
		// (repo slug) is shared by every concurrent run on this repo and
		// cloud titles are model-generated, so "one new recent task" may
		// be a neighbor's submission inside the window. Adopting it would
		// apply and commit a foreign diff as this task's — so candidates
		// are reported, never adopted (never-guess).
		r.log("warn", fmt.Sprintf("cloud exec failed (%v); reconciling for an orphan submission", err))
		if !snapshotOK {
			return failf(codeSubmit, "",
				"cloud exec failed: %v; the pre-submit snapshot also failed, so an orphan submission cannot be identified — verify with `codex cloud list --env %s` before retrying",
				err, r.env)
		}
		if !sleepCtx(ctx, r.s.pollInterval) {
			return nil
		}
		tasks, lerr := r.listAll(ctx)
		if lerr != nil {
			return failf(codeSubmit, "", "cloud exec failed: %v; orphan reconcile list also failed: %v", err, lerr)
		}
		var candidates []cloud.Task
		for _, t := range tasks {
			if seen[t.ID] {
				continue
			}
			ts, ok := t.UpdatedAtTime()
			if !ok || ts.Before(submitMark.Add(-orphanSkew)) {
				continue
			}
			candidates = append(candidates, t)
		}
		if len(candidates) == 0 {
			return failf(codeSubmit, "", "cloud exec failed and no orphan submission was found: %v", err)
		}
		ids := make([]string, len(candidates))
		for i, t := range candidates {
			ids[i] = t.ID
		}
		return failf(codeSubmitAmbig, "",
			"cloud exec failed and %d recent task(s) not in the pre-submit snapshot were found; the env label is shared, so none can be confirmed as this run's submission — never guessing: %s (underlying: %v)",
			len(candidates), strings.Join(ids, ", "), err)
	}

	r.sid = sub.ID
	r.url = sub.URL
	r.adoptNote = "Submitted"
	r.saveMarker(ctx, hash)
	return nil
}

// saveMarker persists the reconcile marker. It runs under WithoutCancel:
// the marker IS the crash-window protection — a cancel arriving between a
// successful submit and this save would otherwise lose the only record that
// the paid task exists, and the next retry would double-submit. The write is
// local and fast; completing it under cancel is strictly safer than skipping.
func (r *runner) saveMarker(ctx context.Context, hash string) {
	ctx = context.WithoutCancel(ctx)
	m := &state.Marker{
		TaskID:       r.sid,
		URL:          r.url,
		PromptSHA256: hash,
		Env:          r.env,
		Branch:       r.branch,
		Land:         r.s.landMode,
		SubmittedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := state.Save(ctx, r.cfg.Worktree, m); err != nil {
		r.log("warn", fmt.Sprintf("marker not persisted (retries may not reconcile): %v", err))
	}
}

// poll is the §5.6 loop: an immediate first tick then every poll interval,
// keepalive as an at-least-one-event-per-window guarantee, the deadline as
// the overall budget, ctx as CANCEL.
func (r *runner) poll(ctx context.Context) (*cloud.Task, *failure) {
	pollT := time.NewTimer(0)
	defer pollT.Stop()
	keepT := time.NewTimer(r.s.keepalive)
	defer keepT.Stop()
	deadlineT := time.NewTimer(r.s.deadline - time.Since(r.start))
	defer deadlineT.Stop()

	var (
		missCount, failCount, stallCount int
		stallStatus, stallUpdated        string
		lastErr                          error
	)

	for {
		select {
		case <-ctx.Done():
			return nil, nil

		case <-deadlineT.C:
			// F5: the run continues upstream; a resume-retry lands the
			// finished work. The deadline never orphans the run.
			return nil, failf(codeDeadline, r.sid, "deadline %s exceeded; task %s last status %q",
				fmtDur(r.s.deadline), r.sid, r.lastStatus)

		case <-keepT.C:
			if since := time.Since(r.lastEmit); since >= r.s.keepalive {
				r.say(fmt.Sprintf("Still waiting on Codex Cloud task %s: status %s, elapsed %s, deadline in %s.",
					r.sid, orUnknown(r.lastStatus), fmtDur(time.Since(r.start)),
					fmtDur(r.s.deadline-time.Since(r.start))))
				keepT.Reset(r.s.keepalive)
			} else {
				keepT.Reset(r.s.keepalive - since)
			}

		case <-pollT.C:
			res, err := r.client.Poll(ctx, r.env, r.sid, r.s.listPages)
			if ctx.Err() != nil {
				return nil, nil
			}
			var task *cloud.Task
			switch {
			case err == nil:
				missCount, failCount = 0, 0
				task = &res.Task
			case errors.Is(err, cloud.ErrTaskNotFound):
				failCount = 0
				missCount++
				if missCount >= missLimit {
					// Fallback: the id may have aged off the pages (§5.6).
					st, sErr := r.client.Status(ctx, r.sid)
					if ctx.Err() != nil {
						return nil, nil
					}
					if sErr != nil {
						return nil, failf(codePoll, r.sid,
							"task %s missing from %d consecutive list polls and status fallback failed: %v",
							r.sid, missCount, sErr)
					}
					missCount = 0
					task = &cloud.Task{ID: r.sid, URL: r.url, Title: st.Title, Status: st.RawStatus}
				}
			default:
				missCount = 0
				failCount++
				lastErr = err
				if failCount >= listFailLimit {
					return nil, failf(codePoll, r.sid, "%d consecutive poll failures; last: %v", failCount, lastErr)
				}
			}

			if task != nil {
				r.lastTask = task
				st := cloud.MapState(task.Status)
				if task.Status != r.lastStatus {
					r.lastStatus = task.Status
					r.say(fmt.Sprintf("Task %s status: %s%s. Elapsed %s.",
						r.sid, task.Status, diffStat(task.Summary), fmtDur(time.Since(r.start))))
				}
				switch st {
				case cloud.StateReady:
					return task, nil
				case cloud.StatePending:
					stallCount, stallStatus, stallUpdated = 0, "", ""
				default:
					// Unknown vocabulary: narrate verbatim, terminate
					// only on a frozen updated_at (C4) or a captured
					// terminal status (V2 — list deliberately empty).
					for _, terminal := range terminalFailureStatuses {
						if task.Status == terminal {
							return nil, failf(codeUpstreamStatus, r.sid,
								"task %s reached terminal status %q", r.sid, task.Status)
						}
					}
					if task.Status == stallStatus && task.UpdatedAt == stallUpdated && task.UpdatedAt != "" {
						stallCount++
					} else {
						stallStatus, stallUpdated, stallCount = task.Status, task.UpdatedAt, 1
					}
					if stallCount >= stallLimit {
						return nil, failf(codeUpstreamStatus, r.sid,
							"task %s reached unhandled status %q and stopped progressing (updated_at frozen for %d polls)",
							r.sid, task.Status, stallCount)
					}
				}
			}
			pollT.Reset(r.s.pollInterval)
		}
	}
}

// listAll walks the cursor up to list-pages pages (snapshot + orphan hunt).
func (r *runner) listAll(ctx context.Context) ([]cloud.Task, error) {
	var all []cloud.Task
	cursor := ""
	for page := 0; page < r.s.listPages; page++ {
		resp, err := r.client.List(ctx, r.env, cursor)
		if err != nil {
			return nil, err
		}
		all = append(all, resp.Tasks...)
		if resp.Cursor == nil || *resp.Cursor == "" {
			break
		}
		cursor = *resp.Cursor
	}
	return all, nil
}

// fail emits the E8/E9 result and returns the exit code. All quoted upstream
// text is reject-phrase-filtered except the deliberate E9 template
// (design.md §4.1); post-submit failures carry the id and the NOT-cancelled
// trailer so retries reconcile instead of duplicating.
func (r *runner) fail(f *failure) int {
	dur := time.Since(r.start).Milliseconds()
	if f.resumeReject {
		r.em.ResultError("", dur, f.msg)
		return ExitError
	}
	text := protocol.RedactPhrases(f.msg, resumeRejectPhrase)
	if f.sid != "" {
		text += fmt.Sprintf("\nTask: %s %s\nThe upstream Codex Cloud run was NOT cancelled and may still complete; a retry will reconcile by task id.",
			f.sid, taskURL(f.sid))
	}
	r.em.ResultError(f.sid, dur, text)
	return ExitError
}

// failOrCancel routes a stage result: CANCEL beats any failure (the daemon
// asked us to go; no result is emitted, exit 143 — design.md §5.1).
func (r *runner) failOrCancel(ctx context.Context, f *failure) int {
	if ctx.Err() != nil {
		return ExitCancel
	}
	if f == nil {
		// Defensive: a nil failure without cancellation is a logic error;
		// surface it rather than exiting silently.
		f = failf(codeConfig, r.sid, "internal error: stage failed without a failure")
	}
	return r.fail(f)
}

// say emits assistant narration; log emits E6. Both feed the keepalive
// window (any emit resets it), and both are reject-phrase-filtered: any
// event may quote upstream subprocess output, and the phrase must appear in
// exactly one template — the deliberate E9, which does not pass through here
// (design.md §4.1).
func (r *runner) say(text string) {
	r.noteEmit(r.em.Assistant(r.sid, protocol.RedactPhrases(text, resumeRejectPhrase)))
}

func (r *runner) log(level, msg string) {
	r.noteEmit(r.em.Log(r.sid, level, protocol.RedactPhrases(msg, resumeRejectPhrase)))
}

// noteEmit feeds the keepalive window and converts a failed WRITE into
// cancellation: stdout's only reader is the daemon, so a write error (EPIPE
// after daemon death) means nobody will ever consume a result — polling on
// for up to the full deadline would leave a zombie shim beside a paid run.
// ErrAfterResult is not a write failure (it is the latch refusing a late
// event) and never cancels.
func (r *runner) noteEmit(err error) {
	if err != nil && !errors.Is(err, protocol.ErrAfterResult) {
		r.cancelRun()
		return
	}
	r.lastEmit = time.Now()
}

// readPromptGuarded applies the 60 s stdin guard (§5.1); every error is F1.
func readPromptGuarded(ctx context.Context, stdin io.Reader) (string, *failure) {
	type res struct {
		text string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		t, err := protocol.ReadPrompt(stdin)
		ch <- res{t, err}
	}()
	select {
	case <-ctx.Done():
		return "", nil
	case <-time.After(promptReadTimeout):
		return "", failf(codeInput, "", "no prompt line on stdin within %s", promptReadTimeout)
	case out := <-ch:
		if out.err != nil {
			return "", failf(codeInput, "", "%v", out.err)
		}
		return out.text, nil
	}
}

const (
	scratchPrefix = "codex-cloud-shim-"
	scratchMaxAge = 24 * time.Hour
)

// sweepStaleScratch best-effort removes scratch dirs older than scratchMaxAge
// left by hard-killed prior runs. Age gates on the dir's own mtime so a
// concurrent live shim's fresh scratch is never touched; every failure is
// ignored (someone else's sweep, permissions, races are all fine).
func sweepStaleScratch(tmpDir string, now time.Time) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), scratchPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < scratchMaxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(tmpDir, e.Name()))
	}
}

// sleepCtx waits d or until cancel; reports false on cancel.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func taskURL(id string) string { return "https://chatgpt.com/codex/tasks/" + id }

// slugComparable reports whether an env value is an owner/repo label the
// identity guard can compare against a checkout's origin slug. Opaque env IDs
// (hex, no slash) carry no repo identity and are exempt from comparison.
func slugComparable(env string) bool { return strings.Contains(env, "/") }

func diffStat(s *cloud.Summary) string {
	if s == nil || (s.FilesChanged == 0 && s.LinesAdded == 0 && s.LinesRemoved == 0) {
		return ""
	}
	return fmt.Sprintf(" (+%d/-%d, %d %s)", s.LinesAdded, s.LinesRemoved, s.FilesChanged, plural(s.FilesChanged, "file"))
}

func plural(n int, unit string) string {
	if n == 1 {
		return unit
	}
	return unit + "s"
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// fmtDur renders durations for narration: sub-second values keep their
// precision, everything else rounds to the second.
func fmtDur(d time.Duration) string {
	if d >= time.Second {
		d = d.Round(time.Second)
	}
	return d.String()
}
