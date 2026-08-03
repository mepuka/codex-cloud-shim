package protocol

import (
	"reflect"
	"testing"
)

// The exact launch line Multica builds (buildClaudeArgs, claude.go:668 —
// docs/multica-claude-contract.md §Invocation), with custom_args appended.
func TestParseArgsMulticaLaunchLine(t *testing.T) {
	argv := []string{
		"-p", "--output-format", "stream-json", "--input-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions", "--disallowedTools", "AskUserQuestion",
		"--strict-mcp-config", "--model", "gpt-5", "--effort", "high", "--max-turns", "40",
		"--resume", "task_e_6a70b48f64648323bba8af2747578941",
		"--mcp-config", "/tmp/mcp.json",
		"--shim-env", "mepuka/tailtalk", "--shim-land=push",
		"--settings", "/tmp/settings.json",
	}
	p, err := ParseArgs(argv)
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if p.Resume != "task_e_6a70b48f64648323bba8af2747578941" {
		t.Errorf("Resume = %q", p.Resume)
	}
	want := map[string]string{"env": "mepuka/tailtalk", "land": "push"}
	if !reflect.DeepEqual(p.Shim, want) {
		t.Errorf("Shim = %v, want %v", p.Shim, want)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", p.Warnings)
	}
	if p.Version || p.Help {
		t.Errorf("Version/Help set on a launch line")
	}
}

func TestParseArgsEveryShimFlag(t *testing.T) {
	names := []string{
		"env", "branch", "land", "attempts", "attempt", "poll-interval",
		"keepalive", "deadline", "exec-timeout", "issue-key", "codex-bin", "list-pages",
	}
	for _, name := range names {
		t.Run(name+" space form", func(t *testing.T) {
			p, err := ParseArgs([]string{"--shim-" + name, "val-" + name})
			if err != nil {
				t.Fatalf("ParseArgs: %v", err)
			}
			if p.Shim[name] != "val-"+name {
				t.Errorf("Shim[%q] = %q", name, p.Shim[name])
			}
		})
		t.Run(name+" equals form", func(t *testing.T) {
			p, err := ParseArgs([]string{"--shim-" + name + "=val-" + name})
			if err != nil {
				t.Fatalf("ParseArgs: %v", err)
			}
			if p.Shim[name] != "val-"+name {
				t.Errorf("Shim[%q] = %q", name, p.Shim[name])
			}
		})
	}
}

func TestParseArgsTable(t *testing.T) {
	tests := []struct {
		name     string
		argv     []string
		want     *ParsedArgs
		wantErr  bool
		warnings int
	}{
		{
			name: "resume equals form",
			argv: []string{"--resume=task_e_abc"},
			want: &ParsedArgs{Resume: "task_e_abc", Shim: map[string]string{}},
		},
		{
			name: "claude value flags both forms ignored",
			argv: []string{"--model=opus", "--max-turns", "10", "--add-dir", "/x"},
			want: &ParsedArgs{Shim: map[string]string{}},
		},
		{
			name: "boolean claude flags consume nothing",
			argv: []string{"--verbose", "positional", "--continue", "--dangerously-skip-permissions", "--include-partial-messages", "--print"},
			want: &ParsedArgs{Shim: map[string]string{}},
		},
		{
			name:     "unknown =-form flag ignored whole with warning (C14)",
			argv:     []string{"--frobnicate=9", "--model", "m"},
			want:     &ParsedArgs{Shim: map[string]string{}},
			warnings: 1,
		},
		{
			name:     "bare unknown flag never consumes the next token (C14)",
			argv:     []string{"--frobnicate", "--resume", "task_e_x"},
			want:     &ParsedArgs{Resume: "task_e_x", Shim: map[string]string{}},
			warnings: 1,
		},
		{
			name:     "single-dash unknown warned",
			argv:     []string{"-z"},
			want:     &ParsedArgs{Shim: map[string]string{}},
			warnings: 1,
		},
		{
			name: "positionals silently ignored",
			argv: []string{"some", "prompt", "words"},
			want: &ParsedArgs{Shim: map[string]string{}},
		},
		{
			name: "version short-circuit",
			argv: []string{"--version"},
			want: &ParsedArgs{Version: true, Shim: map[string]string{}},
		},
		{
			name: "help short-circuit",
			argv: []string{"--help"},
			want: &ParsedArgs{Help: true, Shim: map[string]string{}},
		},
		{
			name: "duplicate shim flag last wins",
			argv: []string{"--shim-land", "report", "--shim-land", "push"},
			want: &ParsedArgs{Shim: map[string]string{"land": "push"}},
		},
		{
			name: "empty value in equals form kept raw",
			argv: []string{"--shim-issue-key="},
			want: &ParsedArgs{Shim: map[string]string{"issue-key": ""}},
		},
		{
			name: "empty argv",
			argv: nil,
			want: &ParsedArgs{Shim: map[string]string{}},
		},
		{name: "unknown shim flag errors (F0)", argv: []string{"--shim-bogus", "x"}, wantErr: true},
		{name: "shim flag missing value errors (F0)", argv: []string{"--shim-env"}, wantErr: true},
		{name: "claude value flag missing value errors", argv: []string{"--model"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParseArgs(tt.argv)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArgs: %v", err)
			}
			if len(p.Warnings) != tt.warnings {
				t.Errorf("Warnings = %v, want %d entries", p.Warnings, tt.warnings)
			}
			p.Warnings = nil
			if !reflect.DeepEqual(p, tt.want) {
				t.Errorf("got %+v, want %+v", p, tt.want)
			}
		})
	}
}
