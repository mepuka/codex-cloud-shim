package cloud

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const probeTaskID = "task_e_6a70b48f64648323bba8af2747578941"

// stubBin resolves testdata/fake-codex/codex (repo root), the executable stub
// reproducing the measured CLI outputs. The real codex is never invoked.
func stubBin(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-codex", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stub codex missing: %v", err)
	}
	return p
}

// newClient wires a Client at the stub with a fresh state dir (list counter +
// calls.log) and a fresh scratch dir per test.
func newClient(t *testing.T, scenario string) (c *Client, stateDir string) {
	t.Helper()
	stateDir = t.TempDir()
	c = &Client{
		Bin:     stubBin(t),
		Scratch: t.TempDir(),
		Env: []string{
			"FAKE_CODEX_SCENARIO=" + scenario,
			"FAKE_CODEX_STATE_DIR=" + stateDir,
		},
		ExecTimeout:  10 * time.Second,
		QueryTimeout: 10 * time.Second,
		ApplyTimeout: 20 * time.Second,
	}
	return c, stateDir
}

func callsLog(t *testing.T, stateDir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stateDir, "calls.log"))
	if err != nil {
		t.Fatalf("read calls.log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// sameDir compares paths through symlinks (macOS TempDir is /var → /private/var).
func sameDir(t *testing.T, got, want string) bool {
	t.Helper()
	rg, err1 := filepath.EvalSymlinks(got)
	rw, err2 := filepath.EvalSymlinks(want)
	if err1 != nil || err2 != nil {
		return got == want
	}
	return rg == rw
}

// runGit is a test-only git helper for the apply fixtures.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initWorktree builds a minimal local repo (no network) for apply tests. The
// happy apply.patch creates docs/PROBE.md, absent here by construction.
func initWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "seed")
	return dir
}

func TestSubmitParsesTaskURL(t *testing.T) {
	c, state := newClient(t, "happy")
	sub, err := c.Submit(context.Background(), "mepuka/codex-cloud-shim", "main", 0, "Create docs/PROBE.md")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if sub.ID != probeTaskID {
		t.Errorf("ID = %q, want %q", sub.ID, probeTaskID)
	}
	if sub.URL != "https://chatgpt.com/codex/tasks/"+probeTaskID {
		t.Errorf("URL = %q", sub.URL)
	}
	calls := callsLog(t, state)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if !strings.Contains(calls[0], "cloud exec --env mepuka/codex-cloud-shim --branch main") {
		t.Errorf("exec argv missing env/branch: %q", calls[0])
	}
	// CWD quarantine: the invocation ran in Scratch, so the measured
	// error.log side effect landed there and nowhere else.
	cwd := strings.SplitN(calls[0], "\t", 2)[0]
	if !sameDir(t, cwd, c.Scratch) {
		t.Errorf("exec CWD = %q, want scratch %q", cwd, c.Scratch)
	}
	if _, err := os.Stat(filepath.Join(c.Scratch, "error.log")); err != nil {
		t.Errorf("stub error.log side effect not in scratch: %v", err)
	}
}

func TestSubmitPassesAttempts(t *testing.T) {
	c, state := newClient(t, "happy")
	if _, err := c.Submit(context.Background(), "o/r", "main", 3, "p"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if calls := callsLog(t, state); !strings.Contains(calls[0], "--attempts 3") {
		t.Errorf("argv missing --attempts 3: %q", calls[0])
	}
}

func TestSubmitEnvNotFound(t *testing.T) {
	c, _ := newClient(t, "env-not-found")
	_, err := c.Submit(context.Background(), "bogus/repo", "main", 0, "p")
	if !errors.Is(err, ErrEnvNotFound) {
		t.Fatalf("err = %v, want ErrEnvNotFound", err)
	}
	if !strings.Contains(err.Error(), "environment 'bogus/repo' not found") {
		t.Errorf("verbatim stderr missing from error: %v", err)
	}
}

func TestPollSequencePendingToReady(t *testing.T) {
	c, _ := newClient(t, "happy")
	ctx := context.Background()
	env := "mepuka/codex-cloud-shim"

	want := []State{StatePending, StatePending, StateReady}
	var last *PollResult
	for i, w := range want {
		res, err := c.Poll(ctx, env, probeTaskID, 1)
		if err != nil {
			t.Fatalf("poll %d: %v", i+1, err)
		}
		if res.State != w {
			t.Errorf("poll %d state = %q, want %q", i+1, res.State, w)
		}
		last = res
	}
	if last.Task.Summary == nil || last.Task.Summary.FilesChanged != 1 ||
		last.Task.Summary.LinesAdded != 3 || last.Task.Summary.LinesRemoved != 0 {
		t.Errorf("ready summary = %+v, want +3/-0 across 1 file", last.Task.Summary)
	}
	// clamp: the last fixture repeats
	res, err := c.Poll(ctx, env, probeTaskID, 1)
	if err != nil || res.State != StateReady {
		t.Errorf("poll 4 = (%v, %v), want ready again", res, err)
	}
}

func TestPollUnknownStateConservatism(t *testing.T) {
	c, _ := newClient(t, "unknown-status")
	res, err := c.Poll(context.Background(), "mepuka/codex-cloud-shim", probeTaskID, 1)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.State != StateUnknown {
		t.Errorf("state = %q, want unknown", res.State)
	}
	if res.Task.Status != "weird_state" {
		t.Errorf("verbatim status = %q, want weird_state", res.Task.Status)
	}
}

func TestPollTaskNotFound(t *testing.T) {
	c, _ := newClient(t, "happy")
	_, err := c.Poll(context.Background(), "mepuka/codex-cloud-shim", "task_e_deadbeef", 1)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestPollFollowsCursor(t *testing.T) {
	c, state := newClient(t, "multipage")
	res, err := c.Poll(context.Background(), "mepuka/codex-cloud-shim", probeTaskID, 2)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.State != StatePending {
		t.Errorf("state = %q, want pending", res.State)
	}
	calls := callsLog(t, state)
	if len(calls) != 2 {
		t.Fatalf("list calls = %d, want 2", len(calls))
	}
	if !strings.Contains(calls[1], "--cursor c1") {
		t.Errorf("second list missing --cursor c1: %q", calls[1])
	}
}

func TestPollPageBound(t *testing.T) {
	c, _ := newClient(t, "multipage")
	_, err := c.Poll(context.Background(), "mepuka/codex-cloud-shim", probeTaskID, 1)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound at page bound", err)
	}
}

func TestStatusFallbackParse(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		state    State
		raw      string
	}{
		{"happy", StatePending, "pending"},
		{"status-ready", StateReady, "ready"},
	} {
		c, _ := newClient(t, tc.scenario)
		res, err := c.Status(context.Background(), probeTaskID)
		if err != nil {
			t.Fatalf("%s: Status: %v", tc.scenario, err)
		}
		if res.State != tc.state || res.RawStatus != tc.raw {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", tc.scenario, res.State, res.RawStatus, tc.state, tc.raw)
		}
		if res.Title != "Create docs/PROBE.md with UTC date" {
			t.Errorf("%s: title = %q", tc.scenario, res.Title)
		}
	}
}

func TestDiffReturnsPatch(t *testing.T) {
	c, _ := newClient(t, "happy")
	diff, err := c.Diff(context.Background(), probeTaskID)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fake-codex", "scenarios", "happy", "diff.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if diff != string(want) {
		t.Errorf("diff mismatch:\n%s", diff)
	}
}

func TestApplyStagesAndScrubsErrorLog(t *testing.T) {
	c, _ := newClient(t, "happy")
	wt := initWorktree(t)
	res, err := c.Apply(context.Background(), wt, probeTaskID, 0)
	if err != nil {
		t.Fatalf("Apply: %v (stderr: %s)", err, res.Stderr)
	}
	staged := runGit(t, wt, "diff", "--cached", "--name-status")
	if staged != "A\tdocs/PROBE.md" {
		t.Errorf("staged = %q, want A\\tdocs/PROBE.md", staged)
	}
	// measured CWD side effect never delivered (absent-before → removed)
	if _, err := os.Stat(filepath.Join(wt, "error.log")); !os.IsNotExist(err) {
		t.Errorf("error.log survived in worktree (stat err = %v)", err)
	}
	exclude, err := os.ReadFile(filepath.Join(wt, ".git", "info", "exclude"))
	if err != nil || !strings.Contains(string(exclude), "error.log") {
		t.Errorf("exclude entry missing (err=%v):\n%s", err, exclude)
	}
	// --attempt passthrough on a second client call form is covered below
}

func TestApplyIdempotentReapply(t *testing.T) {
	c, _ := newClient(t, "happy")
	wt := initWorktree(t)
	ctx := context.Background()
	if _, err := c.Apply(ctx, wt, probeTaskID, 0); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, err := c.Apply(ctx, wt, probeTaskID, 0); err != nil {
		t.Fatalf("re-Apply not idempotent: %v", err)
	}
}

func TestApplyPassesAttempt(t *testing.T) {
	c, state := newClient(t, "happy")
	wt := initWorktree(t)
	if _, err := c.Apply(context.Background(), wt, probeTaskID, 2); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	calls := callsLog(t, state)
	if !strings.Contains(calls[0], "cloud apply "+probeTaskID+" --attempt 2") {
		t.Errorf("argv missing --attempt 2: %q", calls[0])
	}
	cwd := strings.SplitN(calls[0], "\t", 2)[0]
	if !sameDir(t, cwd, wt) {
		t.Errorf("apply CWD = %q, want worktree %q", cwd, wt)
	}
}

func TestApplyConflictParsesCountsTreeUntouched(t *testing.T) {
	c, _ := newClient(t, "apply-conflict")
	wt := initWorktree(t)
	before := runGit(t, wt, "status", "--porcelain")
	res, err := c.Apply(context.Background(), wt, probeTaskID, 0)
	if !errors.Is(err, ErrApplyFailed) {
		t.Fatalf("err = %v, want ErrApplyFailed", err)
	}
	if !res.HasCounts || res.Applied != 0 || res.Skipped != 1 || res.Conflicts != 0 {
		t.Errorf("counts = %+v, want applied=0 skipped=1 conflicts=0", res)
	}
	if !strings.Contains(res.Stderr, "(applied=0, skipped=1, conflicts=0)") {
		t.Errorf("stderr not carried: %q", res.Stderr)
	}
	after := runGit(t, wt, "status", "--porcelain")
	if before != after {
		t.Errorf("tree changed on conflict:\nbefore %q\nafter %q", before, after)
	}
	if _, err := os.Stat(filepath.Join(wt, "error.log")); !os.IsNotExist(err) {
		t.Errorf("error.log survived failed apply (stat err = %v)", err)
	}
}

func TestApplyPreexistingErrorLogTruncated(t *testing.T) {
	c, _ := newClient(t, "happy")
	wt := initWorktree(t)
	prior := []byte("prior\n")
	if err := os.WriteFile(filepath.Join(wt, "error.log"), prior, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(context.Background(), wt, probeTaskID, 0); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "error.log"))
	if err != nil {
		t.Fatalf("pre-existing error.log removed: %v", err)
	}
	if string(got) != string(prior) {
		t.Errorf("error.log = %q, want restored %q", got, prior)
	}
}

func TestApplyTrackedErrorLogRestored(t *testing.T) {
	c, _ := newClient(t, "happy")
	wt := initWorktree(t)
	committed := []byte("committed content\n")
	if err := os.WriteFile(filepath.Join(wt, "error.log"), committed, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt, "add", "error.log")
	runGit(t, wt, "commit", "-m", "track error.log")
	if _, err := c.Apply(context.Background(), wt, probeTaskID, 0); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "error.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(committed) {
		t.Errorf("tracked error.log = %q, want checkout-restored %q", got, committed)
	}
}

func TestMapState(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want State
	}{
		{"pending", StatePending},
		{"ready", StateReady},
		{"weird_state", StateUnknown},
		{"", StateUnknown},
		{"READY", StateUnknown}, // case-sensitive: never guess
	} {
		if got := MapState(tc.in); got != tc.want {
			t.Errorf("MapState(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUpdatedAtTimeDegrades(t *testing.T) {
	if _, ok := (Task{UpdatedAt: "not-a-time"}).UpdatedAtTime(); ok {
		t.Error("unparsable updated_at reported ok")
	}
	ts, ok := (Task{UpdatedAt: "2026-08-03T15:32:33.502743959Z"}).UpdatedAtTime()
	if !ok || ts.IsZero() {
		t.Errorf("measured timestamp failed to parse: %v %v", ts, ok)
	}
}

func TestLoginStatus(t *testing.T) {
	c, _ := newClient(t, "happy")
	out, err := c.LoginStatus(context.Background())
	if err != nil {
		t.Fatalf("LoginStatus: %v", err)
	}
	if out != "Logged in using ChatGPT" {
		t.Errorf("out = %q", out)
	}
}
