// End-to-end tests: the compiled binary, launched with the Multica argv
// (buildClaudeArgs, claude.go:668 — docs/multica-claude-contract.md
// §Invocation) against the fake-codex stub (testdata/fake-codex) and a real
// local git fixture. The real codex binary is never invoked (fixed
// decision). No network, no ports.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var shimBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "shim-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	shimBin = filepath.Join(tmp, "codex-cloud-shim")
	build := exec.Command("go", "build", "-o", shimBin, ".")
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build shim:", err)
		os.RemoveAll(tmp)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

const (
	taskID     = "task_e_6a70b48f64648323bba8af2747578941"
	taskURLStr = "https://chatgpt.com/codex/tasks/" + taskID
	envLabel   = "mepuka/codex-cloud-shim"
	promptText = "Create docs/PROBE.md with UTC date.\n\nIssue: DEV-123"
	cloudTitle = "Create docs/PROBE.md with UTC date"
)

// event mirrors the wire shape for assertions.
type event struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Message   *struct {
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Log *struct {
		Level   string `json:"level"`
		Message string `json:"message"`
	} `json:"log"`
	IsError  *bool  `json:"is_error"`
	NumTurns int    `json:"num_turns"`
	Result   string `json:"result"`
}

func (e event) text() string {
	if e.Message == nil || len(e.Message.Content) == 0 {
		return ""
	}
	return e.Message.Content[0].Text
}

func stubPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-codex", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, errBuf.String())
	}
	return strings.TrimSpace(out.String())
}

// gitFixture builds a bare origin plus a worktree clone checked out on an
// agent branch tracking origin/main — the Multica task-worktree shape.
func gitFixture(t *testing.T) (worktree, origin string) {
	t.Helper()
	// EvalSymlinks: on macOS t.TempDir() is under /var, a symlink to
	// /private/var; the stub logs the physical $PWD, so compare physical.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin = filepath.Join(root, "origin.git")
	runGit(t, root, "init", "--bare", "--initial-branch=main", origin)

	worktree = filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktree, "init", "--initial-branch=main")
	runGit(t, worktree, "config", "user.name", "E2E Test")
	runGit(t, worktree, "config", "user.email", "e2e@example.invalid")
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "initial")
	runGit(t, worktree, "remote", "add", "origin", origin)
	runGit(t, worktree, "push", "-u", "origin", "main")
	runGit(t, worktree, "checkout", "-b", "agent/test/task-1")
	runGit(t, worktree, "branch", "--set-upstream-to=origin/main")
	return worktree, origin
}

// multicaArgv is the daemon's launch line plus shim custom_args (which
// arrive after the claude flags, fixed decision).
func multicaArgv(t *testing.T, extra ...string) []string {
	argv := []string{
		"-p", "--output-format", "stream-json", "--input-format", "stream-json",
		"--verbose", "--permission-mode", "bypassPermissions",
		"--disallowedTools", "AskUserQuestion",
	}
	argv = append(argv,
		"--shim-codex-bin", stubPath(t),
		"--shim-env", envLabel,
		"--shim-poll-interval", "30ms",
		"--shim-keepalive", "10s",
		"--shim-deadline", "30s",
	)
	return append(argv, extra...)
}

func promptLine(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
	return string(b) + "\n"
}

type shimResult struct {
	events   []event
	rawLines []string
	exitCode int
}

// runShim launches the compiled binary in worktree against the stub
// scenario and keeps stdin open (as the daemon does) until it exits.
func runShim(t *testing.T, worktree, scenario, stateDir string, argv []string) shimResult {
	t.Helper()
	cmd := exec.Command(shimBin, argv...)
	cmd.Dir = worktree
	cmd.Env = append(os.Environ(),
		"FAKE_CODEX_SCENARIO="+scenario,
		"FAKE_CODEX_STATE_DIR="+stateDir,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte(promptLine(promptText))); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	stdin.Close()
	code := 0
	if waitErr != nil {
		ee, ok := waitErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("wait: %v\nstderr: %s", waitErr, stderr.String())
		}
		code = ee.ExitCode()
	}
	return shimResult{
		events:   parseEvents(t, stdout.String()),
		rawLines: nonEmptyLines(stdout.String()),
		exitCode: code,
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func parseEvents(t *testing.T, stdout string) []event {
	t.Helper()
	var evs []event
	for _, l := range nonEmptyLines(stdout) {
		var e event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", l, err)
		}
		evs = append(evs, e)
	}
	return evs
}

// checkStream applies the shared stream checker (design.md §10.4): line-JSON
// throughout, first task-carrying event is system init, exactly one result
// and it is last, no user/control_request, session_id constant.
func checkStream(t *testing.T, evs []event) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no events emitted")
	}
	results := 0
	sid := ""
	sawSystem := false
	for i, e := range evs {
		switch e.Type {
		case "user", "control_request":
			t.Fatalf("forbidden event type %q at index %d", e.Type, i)
		case "result":
			results++
			if i != len(evs)-1 {
				t.Fatalf("result event at index %d is not last of %d", i, len(evs))
			}
		case "system":
			sawSystem = true
			if e.SessionID == "" {
				t.Fatal("system event without session_id")
			}
		}
		if e.SessionID != "" && !sawSystem {
			t.Fatalf("event %d carries session_id %q before the system init", i, e.SessionID)
		}
		if e.SessionID != "" {
			if sid == "" {
				sid = e.SessionID
			} else if e.SessionID != sid {
				t.Fatalf("session_id changed mid-run: %q then %q", sid, e.SessionID)
			}
		}
		if strings.Contains(e.Result, "async_launched") || strings.Contains(e.text(), "async_launched") {
			t.Fatalf("event %d contains async_launched", i)
		}
	}
	if results != 1 {
		t.Fatalf("want exactly one result event, got %d", results)
	}
}

// readCallsLog returns one entry per stub invocation. The stub writes
// "CWD\targv" per call, but a multi-line prompt inside the exec argv spans
// lines — continuation lines (no tab) are folded into the previous entry.
func readCallsLog(t *testing.T, stateDir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stateDir, "calls.log"))
	if err != nil {
		t.Fatalf("calls.log: %v", err)
	}
	var entries []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.Contains(l, "\t") || len(entries) == 0 {
			entries = append(entries, l)
		} else {
			entries[len(entries)-1] += "\n" + l
		}
	}
	return entries
}

func TestE2EHappyCommitEventStream(t *testing.T) {
	worktree, origin := gitFixture(t)
	stateDir := t.TempDir()

	res := runShim(t, worktree, "happy", stateDir, multicaArgv(t))
	if res.exitCode != 0 {
		t.Fatalf("exit code %d, want 0\nevents: %s", res.exitCode, strings.Join(res.rawLines, "\n"))
	}
	checkStream(t, res.events)

	// The exact ordered stream: env log, branch log, system init,
	// submitted, pending, ready, landing, apply progress, result.
	type want struct {
		typ, subtype, contains string
	}
	wants := []want{
		{"log", "", "env " + envLabel + " (rule: --shim-env)"},
		{"log", "", "base branch main (rule: upstream of HEAD)"},
		{"system", "init", ""},
		{"assistant", "", "Submitted Codex Cloud task " + taskID + " (env " + envLabel + ", base branch main)"},
		{"assistant", "", "Task " + taskID + " status: pending"},
		{"assistant", "", "Task " + taskID + " status: ready (+3/-0, 1 file)"},
		{"assistant", "", "Task ready; landing with mode commit (apply → commit)"},
		{"log", "", "applying diff to worktree"},
		{"result", "success", ""},
	}
	if len(res.events) != len(wants) {
		t.Fatalf("got %d events, want %d:\n%s", len(res.events), len(wants), strings.Join(res.rawLines, "\n"))
	}
	for i, w := range wants {
		e := res.events[i]
		if e.Type != w.typ {
			t.Fatalf("event %d type = %q, want %q (line: %s)", i, e.Type, w.typ, res.rawLines[i])
		}
		if w.subtype != "" && e.Subtype != w.subtype {
			t.Fatalf("event %d subtype = %q, want %q", i, e.Subtype, w.subtype)
		}
		body := e.text()
		if e.Log != nil {
			body = e.Log.Message
		}
		if w.contains != "" && !strings.Contains(body, w.contains) {
			t.Fatalf("event %d body %q does not contain %q", i, body, w.contains)
		}
	}

	// The two pre-task log events must not carry a session id; everything
	// from the system event on must carry the task id.
	for i, e := range res.events {
		if i < 2 && e.SessionID != "" {
			t.Fatalf("pre-submit event %d carries session_id %q", i, e.SessionID)
		}
		if i >= 2 && e.SessionID != taskID {
			t.Fatalf("event %d session_id = %q, want %q", i, e.SessionID, taskID)
		}
	}

	// Result body: completion report with title, URL, diff stat, landed
	// commit, file list.
	final := res.events[len(res.events)-1]
	if final.IsError == nil || *final.IsError {
		t.Fatalf("result is_error = %v, want false", final.IsError)
	}
	if final.NumTurns != 1 {
		t.Fatalf("result num_turns = %d, want 1", final.NumTurns)
	}
	for _, frag := range []string{
		"Codex Cloud task " + taskID + " completed.",
		"Title: " + cloudTitle,
		"URL: " + taskURLStr,
		"Diff: +3/-0 across 1 file",
		"Landed: commit ",
		"(DEV-123)",
		"Files:\nA\tdocs/PROBE.md",
	} {
		if !strings.Contains(final.Result, frag) {
			t.Fatalf("result %q missing %q", final.Result, frag)
		}
	}

	// Repo state: one new commit with the issue key and all three trailers,
	// clean worktree, error.log never delivered, branch not pushed.
	msg := runGit(t, worktree, "log", "-1", "--format=%B")
	for _, frag := range []string{
		cloudTitle + " (DEV-123)",
		"Codex-Cloud-Task: " + taskID,
		"Codex-Cloud-URL: " + taskURLStr,
		"Codex-Cloud-Base: main",
	} {
		if !strings.Contains(msg, frag) {
			t.Fatalf("commit message %q missing %q", msg, frag)
		}
	}
	if st := runGit(t, worktree, "status", "--porcelain"); st != "" {
		t.Fatalf("worktree not clean after landing:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(worktree, "error.log")); !os.IsNotExist(err) {
		t.Fatalf("error.log was delivered into the worktree (stat err: %v)", err)
	}
	if refs := runGit(t, worktree, "ls-remote", "--heads", origin, "agent/test/task-1"); refs != "" {
		t.Fatalf("agent branch was pushed in commit mode: %s", refs)
	}

	// Subprocess discipline: exec carried --env and --branch main; every
	// codex call ran outside the worktree except apply; no unknown verbs.
	calls := readCallsLog(t, stateDir)
	sawExec, sawApply := false, false
	for _, line := range calls {
		cwd, argvStr, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("malformed calls.log line %q", line)
		}
		verbOK := false
		for _, v := range []string{"login status", "cloud exec", "cloud list", "cloud status", "cloud diff", "cloud apply"} {
			if strings.HasPrefix(argvStr, v) {
				verbOK = true
			}
		}
		if !verbOK {
			t.Fatalf("unexpected codex invocation %q (cancel verb or drift)", argvStr)
		}
		if strings.HasPrefix(argvStr, "cloud apply") {
			sawApply = true
			if cwd != worktree {
				t.Fatalf("apply ran in %q, want worktree %q", cwd, worktree)
			}
			continue
		}
		if cwd == worktree {
			t.Fatalf("non-apply call %q ran in the worktree (error.log hazard)", argvStr)
		}
		if strings.HasPrefix(argvStr, "cloud exec") {
			sawExec = true
			if !strings.Contains(argvStr, "--env "+envLabel) || !strings.Contains(argvStr, "--branch main") {
				t.Fatalf("exec argv missing env/branch: %q", argvStr)
			}
		}
	}
	if !sawExec || !sawApply {
		t.Fatalf("calls.log missing exec (%v) or apply (%v):\n%s", sawExec, sawApply, strings.Join(calls, "\n"))
	}
}

// seedMarker writes the reconcile marker for promptText: same prompt hash,
// same task id the daemon passes back as --resume (design.md §5.3/§5.4).
func seedMarker(t *testing.T, worktree string) {
	t.Helper()
	sum := sha256.Sum256([]byte(promptText))
	marker := map[string]string{
		"task_id":       taskID,
		"url":           taskURLStr,
		"prompt_sha256": hex.EncodeToString(sum[:]),
		"env":           envLabel,
		"branch":        "main",
		"land":          "commit",
		"submitted_at":  "2026-08-03T15:32:29Z",
	}
	mb, _ := json.Marshal(marker)
	mdir := filepath.Join(worktree, ".git", "codex-cloud-shim")
	if err := os.MkdirAll(mdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mdir, "state.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestE2EResumeAdoptNoResubmit(t *testing.T) {
	worktree, _ := gitFixture(t)
	stateDir := t.TempDir()
	seedMarker(t, worktree)

	res := runShim(t, worktree, "happy", stateDir, multicaArgv(t, "--resume", taskID))
	if res.exitCode != 0 {
		t.Fatalf("exit code %d, want 0\nevents: %s", res.exitCode, strings.Join(res.rawLines, "\n"))
	}
	checkStream(t, res.events)

	resuming := false
	for _, e := range res.events {
		if strings.Contains(e.text(), "Resuming Codex Cloud task "+taskID) {
			resuming = true
		}
	}
	if !resuming {
		t.Fatalf("no Resuming narration in stream:\n%s", strings.Join(res.rawLines, "\n"))
	}
	final := res.events[len(res.events)-1]
	if final.Type != "result" || final.Subtype != "success" {
		t.Fatalf("final event is %s/%s, want result/success", final.Type, final.Subtype)
	}
	for _, line := range readCallsLog(t, stateDir) {
		if strings.Contains(line, "cloud exec") {
			t.Fatalf("resume retry re-submitted: %q", line)
		}
	}
	// The adopted run still lands.
	msg := runGit(t, worktree, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "Codex-Cloud-Task: "+taskID) {
		t.Fatalf("adopted run did not land a commit; last message: %q", msg)
	}
}

// TestE2EFastExit: SIGTERM and stdin-close mid-poll exit 143 within 2 s,
// emit no result, and never touch the upstream run (no cancel verb in
// calls.log) — design.md §5.1, test plan item 14.
func TestE2EFastExit(t *testing.T) {
	for _, mode := range []string{"stdin-close", "sigterm"} {
		t.Run(mode, func(t *testing.T) {
			worktree, _ := gitFixture(t)
			stateDir := t.TempDir()

			cmd := exec.Command(shimBin, multicaArgv(t)...)
			cmd.Dir = worktree
			cmd.Env = append(os.Environ(),
				"FAKE_CODEX_SCENARIO=pending-forever",
				"FAKE_CODEX_STATE_DIR="+stateDir,
			)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if _, err := stdin.Write([]byte(promptLine(promptText))); err != nil {
				t.Fatal(err)
			}

			lines := make(chan string, 64)
			go func() {
				sc := bufio.NewScanner(stdout)
				for sc.Scan() {
					lines <- sc.Text()
				}
				close(lines)
			}()

			// Wait until the shim is demonstrably mid-poll.
			var seen []string
			deadline := time.After(10 * time.Second)
			pending := false
			for !pending {
				select {
				case l, ok := <-lines:
					if !ok {
						t.Fatalf("stdout closed before a pending poll:\n%s", strings.Join(seen, "\n"))
					}
					seen = append(seen, l)
					if strings.Contains(l, "status: pending") {
						pending = true
					}
				case <-deadline:
					t.Fatalf("never saw a pending poll:\n%s", strings.Join(seen, "\n"))
				}
			}

			killedAt := time.Now()
			switch mode {
			case "stdin-close":
				stdin.Close()
			case "sigterm":
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}

			waitErr := cmd.Wait()
			elapsed := time.Since(killedAt)
			for l := range lines {
				seen = append(seen, l)
			}
			if mode == "sigterm" {
				stdin.Close()
			}

			ee, ok := waitErr.(*exec.ExitError)
			if !ok {
				t.Fatalf("want exit error 143, got %v", waitErr)
			}
			if code := ee.ExitCode(); code != 143 {
				t.Fatalf("exit code %d, want 143\n%s", code, strings.Join(seen, "\n"))
			}
			if elapsed > 2500*time.Millisecond {
				t.Fatalf("exit took %s, want ≤ ~2s", elapsed)
			}
			for _, l := range seen {
				var e event
				if err := json.Unmarshal([]byte(l), &e); err != nil {
					t.Fatalf("non-JSON stdout line %q: %v", l, err)
				}
				if e.Type == "result" {
					t.Fatalf("cancelled run emitted a result: %s", l)
				}
			}
			// The upstream run must be untouched: only known read/submit
			// verbs, never a cancel-shaped invocation.
			for _, line := range readCallsLog(t, stateDir) {
				_, argvStr, _ := strings.Cut(line, "\t")
				okVerb := false
				for _, v := range []string{"login status", "cloud exec", "cloud list", "cloud status"} {
					if strings.HasPrefix(argvStr, v) {
						okVerb = true
					}
				}
				if !okVerb {
					t.Fatalf("unexpected codex invocation after cancel: %q", argvStr)
				}
			}
		})
	}
}

// finalResult returns the last event, asserting it is a result event.
func finalResult(t *testing.T, res shimResult) event {
	t.Helper()
	if len(res.events) == 0 {
		t.Fatal("no events emitted")
	}
	final := res.events[len(res.events)-1]
	if final.Type != "result" {
		t.Fatalf("final event is %q, want result:\n%s", final.Type, strings.Join(res.rawLines, "\n"))
	}
	return final
}

// TestE2EEnvNotFound: the measured fail-fast `environment '<x>' not found`
// surfaces verbatim as F4a with no session_id and no marker written
// (design.md §5.5, test plan item 9).
func TestE2EEnvNotFound(t *testing.T) {
	worktree, _ := gitFixture(t)
	stateDir := t.TempDir()

	res := runShim(t, worktree, "env-not-found", stateDir, multicaArgv(t))
	if res.exitCode != 1 {
		t.Fatalf("exit code %d, want 1\n%s", res.exitCode, strings.Join(res.rawLines, "\n"))
	}
	checkStream(t, res.events)
	final := finalResult(t, res)
	if final.Subtype != "error_during_execution" || final.IsError == nil || !*final.IsError {
		t.Fatalf("final = %s/%s is_error=%v, want error result", final.Type, final.Subtype, final.IsError)
	}
	if final.SessionID != "" {
		t.Fatalf("F4a carries session_id %q, want none (no task exists)", final.SessionID)
	}
	for _, frag := range []string{"E_ENV_NOT_FOUND", "environment 'bogus/repo' not found"} {
		if !strings.Contains(final.Result, frag) {
			t.Fatalf("result %q missing %q", final.Result, frag)
		}
	}
	if _, err := os.Stat(filepath.Join(worktree, ".git", "codex-cloud-shim", "state.json")); !os.IsNotExist(err) {
		t.Fatalf("marker was written on a failed submit (stat err: %v)", err)
	}
	execs := 0
	for _, line := range readCallsLog(t, stateDir) {
		if strings.Contains(line, "cloud exec") {
			execs++
		}
	}
	if execs != 1 {
		t.Fatalf("want exactly one exec attempt, got %d", execs)
	}
}

// TestE2EUnknownStatusStall: an unmeasured status is narrated verbatim and
// terminates as F8 only via the frozen-updated_at stall rule (design.md C4,
// test plan item 10).
func TestE2EUnknownStatusStall(t *testing.T) {
	worktree, _ := gitFixture(t)
	stateDir := t.TempDir()

	res := runShim(t, worktree, "unknown-status", stateDir, multicaArgv(t))
	if res.exitCode != 1 {
		t.Fatalf("exit code %d, want 1\n%s", res.exitCode, strings.Join(res.rawLines, "\n"))
	}
	checkStream(t, res.events)
	narrated := false
	for _, e := range res.events {
		if strings.Contains(e.text(), "status: weird_state") {
			narrated = true
		}
	}
	if !narrated {
		t.Fatalf("verbatim unknown status was not narrated:\n%s", strings.Join(res.rawLines, "\n"))
	}
	final := finalResult(t, res)
	if final.SessionID != taskID {
		t.Fatalf("F8 session_id = %q, want %q", final.SessionID, taskID)
	}
	for _, frag := range []string{
		"E_UPSTREAM_STATUS",
		`unhandled status "weird_state"`,
		"NOT cancelled",
	} {
		if !strings.Contains(final.Result, frag) {
			t.Fatalf("result %q missing %q", final.Result, frag)
		}
	}
}

// TestE2EDeadline: a forever-pending task hits the overall deadline as F5
// with the id and the NOT-cancelled trailer — the run continues upstream
// (design.md §5.6, test plan item 10 moving-variant backstop).
func TestE2EDeadline(t *testing.T) {
	worktree, _ := gitFixture(t)
	stateDir := t.TempDir()

	res := runShim(t, worktree, "pending-forever", stateDir, multicaArgv(t, "--shim-deadline", "1s"))
	if res.exitCode != 1 {
		t.Fatalf("exit code %d, want 1\n%s", res.exitCode, strings.Join(res.rawLines, "\n"))
	}
	checkStream(t, res.events)
	final := finalResult(t, res)
	if final.SessionID != taskID {
		t.Fatalf("F5 session_id = %q, want %q", final.SessionID, taskID)
	}
	for _, frag := range []string{"E_DEADLINE", "deadline 1s exceeded", "NOT cancelled"} {
		if !strings.Contains(final.Result, frag) {
			t.Fatalf("result %q missing %q", final.Result, frag)
		}
	}
}

// TestE2EReconcileAdoptWalksCursor: a fresh launch with a matching marker
// adopts the existing task, and the id hunt follows the list cursor onto the
// second page (design.md §5.3, test plan items 4/8 plus the §5.6 pagination
// bound).
func TestE2EReconcileAdoptWalksCursor(t *testing.T) {
	worktree, _ := gitFixture(t)
	stateDir := t.TempDir()
	seedMarker(t, worktree)

	res := runShim(t, worktree, "multipage", stateDir, multicaArgv(t))
	if res.exitCode != 0 {
		t.Fatalf("exit code %d, want 0\n%s", res.exitCode, strings.Join(res.rawLines, "\n"))
	}
	checkStream(t, res.events)
	adopted := false
	for _, e := range res.events {
		if strings.Contains(e.text(), "Adopted existing Codex Cloud task "+taskID) {
			adopted = true
		}
	}
	if !adopted {
		t.Fatalf("no Adopted existing narration:\n%s", strings.Join(res.rawLines, "\n"))
	}
	cursorWalked := false
	for _, line := range readCallsLog(t, stateDir) {
		if strings.Contains(line, "cloud exec") {
			t.Fatalf("reconcile adopt re-submitted: %q", line)
		}
		if strings.Contains(line, "cloud list") && strings.Contains(line, "--cursor c1") {
			cursorWalked = true
		}
	}
	if !cursorWalked {
		t.Fatal("id hunt never followed the list cursor onto page 2")
	}
	msg := runGit(t, worktree, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "Codex-Cloud-Task: "+taskID) {
		t.Fatalf("adopted run did not land a commit; last message: %q", msg)
	}
}

// TestE2EStatusFallbackLands: the id aged off every list page for 3
// consecutive polls, the `codex cloud status` bracket-parse fallback reports
// READY, and the run lands normally, deciding emptiness by fetching the diff
// (design.md §5.6/§5.7 step 1).
func TestE2EStatusFallbackLands(t *testing.T) {
	worktree, _ := gitFixture(t)
	stateDir := t.TempDir()

	res := runShim(t, worktree, "status-ready", stateDir, multicaArgv(t))
	if res.exitCode != 0 {
		t.Fatalf("exit code %d, want 0\n%s", res.exitCode, strings.Join(res.rawLines, "\n"))
	}
	checkStream(t, res.events)
	final := finalResult(t, res)
	if final.Subtype != "success" {
		t.Fatalf("final subtype %q, want success:\n%s", final.Subtype, final.Result)
	}
	statusCalled := false
	for _, line := range readCallsLog(t, stateDir) {
		if strings.Contains(line, "cloud status "+taskID) {
			statusCalled = true
		}
	}
	if !statusCalled {
		t.Fatal("status fallback was never invoked")
	}
	msg := runGit(t, worktree, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "Codex-Cloud-Task: "+taskID) {
		t.Fatalf("fallback run did not land a commit; last message: %q", msg)
	}
}

// TestE2EApplyConflict: the measured conflict shape (exit 1 plus the counts
// line) is F10 claiming tree untouched, and the worktree really is untouched
// with error.log scrubbed (design.md §5.7 step 3, test plan item 11).
func TestE2EApplyConflict(t *testing.T) {
	worktree, _ := gitFixture(t)
	stateDir := t.TempDir()
	before := runGit(t, worktree, "rev-parse", "HEAD")

	res := runShim(t, worktree, "apply-conflict", stateDir, multicaArgv(t))
	if res.exitCode != 1 {
		t.Fatalf("exit code %d, want 1\n%s", res.exitCode, strings.Join(res.rawLines, "\n"))
	}
	checkStream(t, res.events)
	final := finalResult(t, res)
	if final.SessionID != taskID {
		t.Fatalf("F10 session_id = %q, want %q", final.SessionID, taskID)
	}
	for _, frag := range []string{
		"E_APPLY_CONFLICT",
		"tree untouched",
		"codex cloud diff " + taskID,
		"NOT cancelled",
	} {
		if !strings.Contains(final.Result, frag) {
			t.Fatalf("result %q missing %q", final.Result, frag)
		}
	}
	if after := runGit(t, worktree, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved across a conflicted apply: %s -> %s", before, after)
	}
	if st := runGit(t, worktree, "status", "--porcelain"); st != "" {
		t.Fatalf("worktree dirty after conflicted apply:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(worktree, "error.log")); !os.IsNotExist(err) {
		t.Fatalf("error.log survived the conflicted apply (stat err: %v)", err)
	}
}
