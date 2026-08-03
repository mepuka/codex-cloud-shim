package run

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mepuka/codex-cloud-shim/internal/protocol"
)

func TestSweepStaleScratchRemovesOnlyOldPrefixed(t *testing.T) {
	tmp := t.TempDir()
	now := time.Now()

	old := filepath.Join(tmp, scratchPrefix+"old")
	fresh := filepath.Join(tmp, scratchPrefix+"fresh")
	foreign := filepath.Join(tmp, "unrelated-dir")
	for _, d := range []string{old, fresh, foreign} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stale := now.Add(-scratchMaxAge - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreign, stale, stale); err != nil {
		t.Fatal(err)
	}

	sweepStaleScratch(tmp, now)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("stale prefixed dir survived the sweep: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh prefixed dir was swept: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign dir was swept: %v", err)
	}
}

// failingWriter errors on every write after the first n bytes-writes succeed.
type failingWriter struct{ writesLeft int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.writesLeft > 0 {
		w.writesLeft--
		return len(p), nil
	}
	return 0, errors.New("broken pipe")
}

func TestNoteEmitCancelsOnWriteFailureOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	em := protocol.NewEmitter(&failingWriter{writesLeft: 1})
	r := &runner{em: em, cancelRun: cancel}

	r.log("info", "first write succeeds") // consumes the working write
	if ctx.Err() != nil {
		t.Fatal("successful emit must not cancel")
	}
	before := r.lastEmit

	r.say("second write fails")
	if ctx.Err() == nil {
		t.Fatal("failed event write must cancel the run")
	}
	if r.lastEmit != before {
		t.Error("failed write must not count as an emit for the keepalive window")
	}
}

func TestNoteEmitIgnoresResultLatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	em := protocol.NewEmitter(&failingWriter{writesLeft: 1})
	if err := em.ResultSuccess("sid", 1, "done"); err != nil {
		t.Fatal(err)
	}
	r := &runner{em: em, cancelRun: cancel}
	r.say("late keepalive tick after the result")
	if ctx.Err() != nil {
		t.Fatal("ErrAfterResult is the latch working, not a write failure; it must not cancel")
	}
}

// --- platform checkout bootstrap --------------------------------------------

func writeResources(t *testing.T, workdir, payload string) {
	t.Helper()
	dir := filepath.Join(workdir, ".multica", "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "resources.json"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepoURLsFromWorkdir(t *testing.T) {
	wd := t.TempDir()
	if urls, err := repoURLsFromWorkdir(wd); err != nil || urls != nil {
		t.Fatalf("missing file must be (nil,nil), got %v %v", urls, err)
	}
	writeResources(t, wd, `{"project_id":"p1","resources":[
		{"resource_type":"github_repo","resource_ref":{"url":"https://github.com/o/r"}},
		{"resource_type":"local_directory","resource_ref":{"path":"/x"}},
		{"resource_type":"github_repo","resource_ref":{"broken":true}}]}`)
	urls, err := repoURLsFromWorkdir(wd)
	if err != nil || len(urls) != 1 || urls[0] != "https://github.com/o/r" {
		t.Fatalf("got %v %v", urls, err)
	}
}

func TestEnsureWorktreeBootstrapsViaMultica(t *testing.T) {
	wd := t.TempDir()
	writeResources(t, wd, `{"project_id":"p1","resources":[
		{"resource_type":"github_repo","resource_ref":{"url":"https://github.com/o/r"}}]}`)

	// Fake multica: `multica repo checkout <url>` creates ./r as a git repo.
	binDir := t.TempDir()
	script := filepath.Join(binDir, "multica")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nmkdir -p r && git -C r init -q\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := &runner{
		cfg: Config{
			Worktree: wd,
			Getenv: func(k string) string {
				if k == "CODEX_CLOUD_SHIM_MULTICA_BIN" {
					return script
				}
				return ""
			},
		},
		em: protocol.NewEmitter(io.Discard),
	}
	if f := r.ensureWorktree(context.Background()); f != nil {
		t.Fatalf("ensureWorktree failed: %+v", f)
	}
	if want := filepath.Join(wd, "r"); r.cfg.Worktree != want {
		t.Fatalf("worktree = %q, want %q", r.cfg.Worktree, want)
	}
}

func TestEnsureWorktreeRefusesAmbiguousResources(t *testing.T) {
	wd := t.TempDir()
	writeResources(t, wd, `{"project_id":"p1","resources":[
		{"resource_type":"github_repo","resource_ref":{"url":"https://github.com/o/a"}},
		{"resource_type":"github_repo","resource_ref":{"url":"https://github.com/o/b"}}]}`)
	r := &runner{cfg: Config{Worktree: wd, Getenv: func(string) string { return "" }},
		em: protocol.NewEmitter(io.Discard)}
	f := r.ensureWorktree(context.Background())
	if f == nil || !strings.Contains(f.msg, "ambiguous") {
		t.Fatalf("want ambiguous refusal, got %+v", f)
	}
}

func TestEnsureWorktreeNoResourcesExplains(t *testing.T) {
	r := &runner{cfg: Config{Worktree: t.TempDir(), Getenv: func(string) string { return "" }},
		em: protocol.NewEmitter(io.Discard)}
	f := r.ensureWorktree(context.Background())
	if f == nil || !strings.Contains(f.msg, "multica repo add") {
		t.Fatalf("want actionable no-resource failure, got %+v", f)
	}
}

func TestFramedPromptWrapsByDefaultAndOffIsVerbatim(t *testing.T) {
	r := &runner{s: &settings{frame: frameCloud}, env: "mepuka/tailtalk", branch: "main"}
	framed := r.framedPrompt("", "Fix the bug in foo.go")
	if !strings.HasSuffix(framed, "--- task ---\n\nFix the bug in foo.go") {
		t.Errorf("raw prompt must close the framed text verbatim, got tail %q", framed[max(0, len(framed)-60):])
	}
	for _, want := range []string{"mepuka/tailtalk", "main", "code change itself"} {
		if !strings.Contains(framed, want) {
			t.Errorf("frame missing %q", want)
		}
	}

	withIssue := r.framedPrompt("The dispatched work item (issue DEV-87):\n\nTitle: T", "dispatch text")
	for _, want := range []string{"issue DEV-87", "context only", "dispatch text", "editing the files in your workspace now"} {
		if !strings.Contains(withIssue, want) {
			t.Errorf("issue-framed prompt missing %q", want)
		}
	}
	// Measured 2026-08-03: the model follows the prompt's tail, so the work
	// item must come AFTER the dispatch message and the prompt must end on
	// the imperative.
	if strings.Index(withIssue, "issue DEV-87") < strings.Index(withIssue, "dispatch text") {
		t.Error("work item must follow the dispatch message, not lead it")
	}

	r.s.frame = frameOff
	if got := r.framedPrompt("", "Fix the bug in foo.go"); got != "Fix the bug in foo.go" {
		t.Errorf("frame off must submit verbatim, got %q", got)
	}
}

func writeTaskContext(t *testing.T, root, issueID string) {
	t.Helper()
	dir := filepath.Join(root, ".multica")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"managed_by":"multica-daemon-task","agent_id":"a","issue_id":"` + issueID + `"}`
	if err := os.WriteFile(filepath.Join(dir, "daemon_task_context.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIssueIDSidecarBeatsPromptLine(t *testing.T) {
	wd := t.TempDir()
	r := &runner{workdirRoot: wd,
		prompt: "Your assigned issue ID is: 99999999-9999-4999-8999-999999999999\n"}
	if got := r.issueID(); got != "99999999-9999-4999-8999-999999999999" {
		t.Errorf("prompt-line fallback failed, got %q", got)
	}
	writeTaskContext(t, wd, "d64774f1-709f-4dee-b3de-0856e46dfdd4")
	if got := r.issueID(); got != "d64774f1-709f-4dee-b3de-0856e46dfdd4" {
		t.Errorf("sidecar must win, got %q", got)
	}
	// CWD already the checkout: sidecar one level up is still found.
	r2 := &runner{workdirRoot: filepath.Join(wd, "repo"), prompt: "no id here"}
	if got := r2.issueID(); got != "d64774f1-709f-4dee-b3de-0856e46dfdd4" {
		t.Errorf("parent sidecar lookup failed, got %q", got)
	}
}

func fakeMultica(t *testing.T, script string) func(string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "multica")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return func(k string) string {
		if k == "CODEX_CLOUD_SHIM_MULTICA_BIN" {
			return bin
		}
		return ""
	}
}

func TestIssueContextOwnershipFailsClosed(t *testing.T) {
	wd := t.TempDir()
	// No sidecar, no id line: ownership turn must fail rather than submit
	// a contentless prompt.
	r := &runner{workdirRoot: wd, s: &settings{frame: frameCloud},
		em:     protocol.NewEmitter(io.Discard),
		prompt: "**Turn mode: Ownership.** etc", cfg: Config{Getenv: func(string) string { return "" }}}
	if _, f := r.issueContext(context.Background()); f == nil || !strings.Contains(f.msg, codeIssueContext) {
		t.Fatalf("want F13 on ownership without issue id, got %+v", f)
	}

	// Id present but the fetch fails: still fail closed on ownership...
	writeTaskContext(t, wd, "d64774f1-709f-4dee-b3de-0856e46dfdd4")
	r.cfg.Getenv = fakeMultica(t, "#!/bin/sh\nexit 1\n")
	if _, f := r.issueContext(context.Background()); f == nil || !strings.Contains(f.msg, codeIssueContext) {
		t.Fatalf("want F13 on ownership fetch failure, got %+v", f)
	}

	// ...but degrade with a warning on a reply turn.
	r.prompt = "**Turn mode: Reply.**\n> the comment"
	if block, f := r.issueContext(context.Background()); f != nil || block != "" {
		t.Fatalf("reply turn must degrade, got block=%q f=%+v", block, f)
	}
}

func TestIssueContextFetchesAndRenders(t *testing.T) {
	wd := t.TempDir()
	writeTaskContext(t, wd, "d64774f1-709f-4dee-b3de-0856e46dfdd4")
	r := &runner{workdirRoot: wd, s: &settings{frame: frameCloud},
		em:     protocol.NewEmitter(io.Discard),
		prompt: "**Turn mode: Ownership.** etc",
		cfg: Config{Getenv: fakeMultica(t,
			"#!/bin/sh\necho '{\"identifier\":\"DEV-87\",\"title\":\"Add CHANGELOG.md\",\"description\":\"Create a CHANGELOG.\"}'\n")}}
	block, f := r.issueContext(context.Background())
	if f != nil {
		t.Fatalf("issueContext failed: %+v", f)
	}
	for _, want := range []string{"issue DEV-87", "Title: Add CHANGELOG.md", "Create a CHANGELOG."} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q in %q", want, block)
		}
	}

	// frame off: no fetch, no block.
	r.s.frame = frameOff
	if block, f := r.issueContext(context.Background()); f != nil || block != "" {
		t.Fatalf("frame off must skip issue context, got %q %+v", block, f)
	}
}

func TestPreflightRefusesBatchShim(t *testing.T) {
	// Any .cmd on PATH triggers the guard regardless of OS — the check is
	// on the resolved extension, not on runtime GOOS.
	binDir := t.TempDir()
	cmdShim := filepath.Join(binDir, "codex.cmd")
	if err := os.WriteFile(cmdShim, []byte("@echo off\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &runner{s: &settings{codexBin: cmdShim}, em: protocol.NewEmitter(io.Discard),
		cfg: Config{Getenv: func(string) string { return "" }}}
	code := r.run(context.Background())
	if code != ExitError {
		t.Fatalf("want ExitError on batch shim, got %d", code)
	}
}

func TestCloseOwnershipMovesToInReviewOnOwnershipOnly(t *testing.T) {
	wd := t.TempDir()
	writeTaskContext(t, wd, "d64774f1-709f-4dee-b3de-0856e46dfdd4")
	argsFile := filepath.Join(wd, "calls.txt")
	getenv := fakeMultica(t, "#!/bin/sh\necho \"$@\" >> "+argsFile+"\n")

	r := &runner{workdirRoot: wd, s: &settings{frame: frameCloud},
		em:     protocol.NewEmitter(io.Discard),
		prompt: "**Turn mode: Ownership.** etc", cfg: Config{Getenv: getenv}}
	r.closeOwnership(context.Background())
	b, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(b), "issue status d64774f1-709f-4dee-b3de-0856e46dfdd4 in_review") {
		t.Fatalf("want in_review status call, got %q (%v)", b, err)
	}

	// Reply turns never touch status.
	os.Remove(argsFile)
	r.prompt = "**Turn mode: Reply.**"
	r.closeOwnership(context.Background())
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("reply turn must not call multica issue status")
	}
}

func TestSettingsFrameValidation(t *testing.T) {
	getenv := func(string) string { return "" }
	for val, wantErr := range map[string]bool{"cloud": false, "off": false, "verbose": true} {
		s, err := newSettings(&protocol.ParsedArgs{Shim: map[string]string{"frame": val}}, getenv)
		if wantErr != (err != nil) {
			t.Errorf("frame=%q: err=%v, wantErr=%v", val, err, wantErr)
		}
		if err == nil && s.frame != val {
			t.Errorf("frame=%q not applied, got %q", val, s.frame)
		}
	}
	s, err := newSettings(&protocol.ParsedArgs{}, getenv)
	if err != nil || s.frame != frameCloud {
		t.Errorf("default frame must be %q, got %q (err %v)", frameCloud, s.frame, err)
	}
}
