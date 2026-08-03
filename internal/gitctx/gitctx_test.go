package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

func configIdentity(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	runGit(t, dir, "config", "commit.gpgsign", "false")
}

// fixture builds a local bare "origin" plus a clone checked out on an agent
// branch — the shape of a Multica task worktree. No network, no ports: the
// remote is a filesystem path.
func fixture(t *testing.T) (bare, clone string) {
	t.Helper()
	root := t.TempDir()
	bare = filepath.Join(root, "origin.git")
	runGit(t, root, "init", "--bare", "-b", "main", bare)

	seed := filepath.Join(root, "seed")
	runGit(t, root, "init", "-b", "main", seed)
	configIdentity(t, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "seed")
	runGit(t, seed, "remote", "add", "origin", bare)
	runGit(t, seed, "push", "-u", "origin", "main")

	clone = filepath.Join(root, "clone")
	runGit(t, root, "clone", bare, clone)
	configIdentity(t, clone)
	runGit(t, clone, "checkout", "-b", "agent/test/task-1")
	return bare, clone
}

func TestParseSlug(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		{in: "git@github.com:mepuka/codex-cloud-shim.git", want: "mepuka/codex-cloud-shim"},
		{in: "git@github.com:mepuka/codex-cloud-shim", want: "mepuka/codex-cloud-shim"},
		{in: "ssh://git@github.com/mepuka/codex-cloud-shim.git", want: "mepuka/codex-cloud-shim"},
		{in: "ssh://git@github.com:22/mepuka/codex-cloud-shim.git", want: "mepuka/codex-cloud-shim"},
		{in: "https://github.com/mepuka/codex-cloud-shim.git", want: "mepuka/codex-cloud-shim"},
		{in: "https://github.com/mepuka/codex-cloud-shim", want: "mepuka/codex-cloud-shim"},
		{in: "https://gitlab.example.com/group/sub/repo.git", want: "sub/repo"}, // last two segments
		{in: "https://github.com/mepuka/codex-cloud-shim/", want: "mepuka/codex-cloud-shim"},
		{in: "", wantErr: true},
		{in: "https://github.com/onlyone", wantErr: true},
		{in: "/local/path/repo", wantErr: true},
	} {
		got, err := ParseSlug(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseSlug(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSlug(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOriginSlug(t *testing.T) {
	_, clone := fixture(t)
	runGit(t, clone, "remote", "set-url", "origin", "git@github.com:mepuka/tailtalk.git")
	got, err := OriginSlug(context.Background(), clone)
	if err != nil {
		t.Fatalf("OriginSlug: %v", err)
	}
	if got != "mepuka/tailtalk" {
		t.Errorf("slug = %q", got)
	}
}

func TestOriginSlugNoRemote(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	if _, err := OriginSlug(context.Background(), dir); err == nil {
		t.Error("want error for missing origin")
	}
}

func TestCurrentBranch(t *testing.T) {
	_, clone := fixture(t)
	got, err := CurrentBranch(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	if got != "agent/test/task-1" {
		t.Errorf("branch = %q", got)
	}
}

func TestBaseBranchUpstreamRule(t *testing.T) {
	_, clone := fixture(t)
	runGit(t, clone, "branch", "--set-upstream-to=origin/main")
	base, rule, err := BaseBranch(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	if base != "main" || rule != "upstream of HEAD" {
		t.Errorf("got (%q, %q), want (main, upstream of HEAD)", base, rule)
	}
}

func TestBaseBranchSelfTrackingFallsThrough(t *testing.T) {
	// An agent branch pushed with -u tracks origin/<itself>: that says
	// nothing about the base, so the chain must fall through to origin/HEAD.
	_, clone := fixture(t)
	runGit(t, clone, "push", "-u", "origin", "agent/test/task-1")
	base, rule, err := BaseBranch(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	if base != "main" || rule != "origin/HEAD" {
		t.Errorf("got (%q, %q), want (main, origin/HEAD)", base, rule)
	}
}

func TestBaseBranchOriginHeadRule(t *testing.T) {
	_, clone := fixture(t) // agent branch, no upstream
	base, rule, err := BaseBranch(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	if base != "main" || rule != "origin/HEAD" {
		t.Errorf("got (%q, %q), want (main, origin/HEAD)", base, rule)
	}
}

func TestBaseBranchShowRefRule(t *testing.T) {
	_, clone := fixture(t)
	runGit(t, clone, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	base, rule, err := BaseBranch(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	if base != "main" || rule != "origin/main exists" {
		t.Errorf("got (%q, %q), want (main, origin/main exists)", base, rule)
	}
}

func TestBaseBranchMasterFallback(t *testing.T) {
	_, clone := fixture(t)
	runGit(t, clone, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	sha := runGit(t, clone, "rev-parse", "refs/remotes/origin/main")
	runGit(t, clone, "update-ref", "refs/remotes/origin/master", sha)
	runGit(t, clone, "update-ref", "-d", "refs/remotes/origin/main")
	base, rule, err := BaseBranch(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	if base != "master" || rule != "origin/master exists" {
		t.Errorf("got (%q, %q), want (master, origin/master exists)", base, rule)
	}
}

func TestBaseBranchExhausted(t *testing.T) {
	_, clone := fixture(t)
	runGit(t, clone, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	runGit(t, clone, "update-ref", "-d", "refs/remotes/origin/main")
	_, _, err := BaseBranch(context.Background(), clone)
	if err == nil {
		t.Fatal("want error when chain exhausted")
	}
	if !strings.Contains(err.Error(), "--shim-branch") {
		t.Errorf("error should point at --shim-branch pin: %v", err)
	}
}

func TestIssueKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"fix the parser (DEV-123) before Friday", "DEV-123"},
		{"DEV-7 then DEV-8", "DEV-7"}, // first match wins
		{"prefix TAILTALK-99 suffix", "TAILTALK-99"},
		{"lowercase dev-123 is not a key", ""},
		{"task_e_6a70b48f64648323bba8af2747578941", ""},
		{"no key here", ""},
		{"X-0 leading zero invalid", ""},
	} {
		if got := IssueKey(tc.in); got != tc.want {
			t.Errorf("IssueKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCommitMessageShapes(t *testing.T) {
	msg, err := (CommitInput{
		Title:    "Create docs/PROBE.md with UTC date",
		IssueKey: "DEV-123",
		TaskID:   "task_e_abc",
		TaskURL:  "https://chatgpt.com/codex/tasks/task_e_abc",
		Base:     "main",
	}).Message()
	if err != nil {
		t.Fatal(err)
	}
	want := "Create docs/PROBE.md with UTC date (DEV-123)\n\n" +
		"Codex-Cloud-Task: task_e_abc\n" +
		"Codex-Cloud-URL: https://chatgpt.com/codex/tasks/task_e_abc\n" +
		"Codex-Cloud-Base: main\n"
	if msg != want {
		t.Errorf("message:\n%q\nwant:\n%q", msg, want)
	}

	// ultimate fallback title
	msg, err = (CommitInput{TaskID: "task_e_abc"}).Message()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg, "codex-cloud: task_e_abc\n") {
		t.Errorf("fallback title wrong: %q", msg)
	}

	if _, err := (CommitInput{}).Message(); err == nil {
		t.Error("empty input should error")
	}
}

func TestCommitStagedCommitsOnlyStaged(t *testing.T) {
	_, clone := fixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(clone, "landed.txt"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "stray.txt"), []byte("stray\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", "landed.txt") // stray.txt deliberately unstaged

	sha, err := CommitStaged(ctx, clone, CommitInput{
		Title:    "Land the cloud diff",
		IssueKey: "DEV-42",
		TaskID:   "task_e_abc",
		TaskURL:  "https://chatgpt.com/codex/tasks/task_e_abc",
		Base:     "main",
	})
	if err != nil {
		t.Fatalf("CommitStaged: %v", err)
	}
	if len(sha) != 40 {
		t.Errorf("sha = %q", sha)
	}
	if subject := runGit(t, clone, "log", "-1", "--format=%s"); subject != "Land the cloud diff (DEV-42)" {
		t.Errorf("subject = %q", subject)
	}
	body := runGit(t, clone, "log", "-1", "--format=%b")
	for _, trailer := range []string{
		"Codex-Cloud-Task: task_e_abc",
		"Codex-Cloud-URL: https://chatgpt.com/codex/tasks/task_e_abc",
		"Codex-Cloud-Base: main",
	} {
		if !strings.Contains(body, trailer) {
			t.Errorf("body missing %q:\n%s", trailer, body)
		}
	}
	if files := runGit(t, clone, "show", "--name-only", "--format=", "HEAD"); files != "landed.txt" {
		t.Errorf("committed files = %q, want only landed.txt", files)
	}
	if status := runGit(t, clone, "status", "--porcelain", "stray.txt"); status != "?? stray.txt" {
		t.Errorf("stray.txt status = %q, want untracked", status)
	}
}

func TestCommitStagedNothingStagedFails(t *testing.T) {
	_, clone := fixture(t)
	if _, err := CommitStaged(context.Background(), clone, CommitInput{Title: "empty"}); err == nil {
		t.Error("want error when nothing is staged")
	}
}

func TestPushOwnBranchOnly(t *testing.T) {
	bare, clone := fixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(clone, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", "work.txt")
	runGit(t, clone, "commit", "-m", "agent work")
	mainBefore := runGit(t, bare, "rev-parse", "refs/heads/main")

	if err := Push(ctx, clone); err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := runGit(t, clone, "rev-parse", "HEAD")
	got := runGit(t, bare, "rev-parse", "refs/heads/agent/test/task-1")
	if got != want {
		t.Errorf("origin agent branch = %s, want %s", got, want)
	}
	if mainAfter := runGit(t, bare, "rev-parse", "refs/heads/main"); mainAfter != mainBefore {
		t.Errorf("main moved: %s -> %s", mainBefore, mainAfter)
	}
	// -u set the upstream to the own branch
	if up := runGit(t, clone, "rev-parse", "--abbrev-ref", "@{u}"); up != "origin/agent/test/task-1" {
		t.Errorf("upstream = %q", up)
	}
}

func TestPushDetachedHeadRefused(t *testing.T) {
	_, clone := fixture(t)
	runGit(t, clone, "checkout", "--detach")
	if err := Push(context.Background(), clone); err == nil {
		t.Error("want refusal on detached HEAD")
	}
}

func TestDiscoverWorktree(t *testing.T) {
	ctx := context.Background()

	t.Run("cwd itself is a repo", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "-q")
		got, rule, err := DiscoverWorktree(ctx, dir)
		if err != nil || got != dir {
			t.Fatalf("got %q %q %v", got, rule, err)
		}
	})

	t.Run("multica workdir layout: brief at root, single checkout below", func(t *testing.T) {
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "CLAUDE.md"), []byte("brief"), 0o644); err != nil {
			t.Fatal(err)
		}
		checkout := filepath.Join(workdir, "repo")
		if err := os.Mkdir(checkout, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, checkout, "init", "-q")
		if err := os.Mkdir(filepath.Join(workdir, "notes"), 0o755); err != nil {
			t.Fatal(err) // a plain sibling dir must not confuse discovery
		}
		got, rule, err := DiscoverWorktree(ctx, workdir)
		if err != nil || got != checkout {
			t.Fatalf("got %q %q %v", got, rule, err)
		}
	})

	t.Run("zero candidates errors", func(t *testing.T) {
		if _, _, err := DiscoverWorktree(ctx, t.TempDir()); err == nil {
			t.Fatal("want error for no checkout anywhere")
		}
	})

	t.Run("two candidates refuse to guess", func(t *testing.T) {
		workdir := t.TempDir()
		for _, name := range []string{"a", "b"} {
			d := filepath.Join(workdir, name)
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
			runGit(t, d, "init", "-q")
		}
		_, _, err := DiscoverWorktree(ctx, workdir)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("want ambiguous error, got %v", err)
		}
	})
}
