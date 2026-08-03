package protocol

import (
	"fmt"
	"strings"
)

// The claude-compatible argv surface (design §2.1). Multica launches the
// shim with the fixed claude flag set (buildClaudeArgs, claude.go:668 —
// docs/multica-claude-contract.md §Invocation); stdlib flag stops at the
// first unknown token, so this is a hand-rolled single-pass scanner with
// arity tables. The tables are data: extending them for a future claude
// flag is one line plus a table-test row.

// claudeValueFlags consume the next token unless given in --flag=value form.
var claudeValueFlags = map[string]bool{
	"--output-format":          true,
	"--input-format":           true,
	"--permission-mode":        true,
	"--disallowedTools":        true,
	"--allowedTools":           true,
	"--model":                  true,
	"--effort":                 true,
	"--max-turns":              true,
	"--resume":                 true,
	"--settings":               true,
	"--mcp-config":             true,
	"--append-system-prompt":   true,
	"--system-prompt":          true,
	"--session-id":             true,
	"--fallback-model":         true,
	"--add-dir":                true,
	"--permission-prompt-tool": true,
}

// claudeBoolFlags consume nothing.
var claudeBoolFlags = map[string]bool{
	"-p":                             true,
	"--print":                        true,
	"--verbose":                      true,
	"--strict-mcp-config":            true,
	"--continue":                     true,
	"--dangerously-skip-permissions": true,
	"--include-partial-messages":     true,
}

// shimFlagNames are the shim's own flags (design §2.2), all value-taking,
// arriving after the claude flags via Multica custom_args. Keys are the name
// after the "--shim-" prefix.
var shimFlagNames = map[string]bool{
	"env":           true,
	"branch":        true,
	"land":          true,
	"attempts":      true,
	"attempt":       true,
	"poll-interval": true,
	"keepalive":     true,
	"deadline":      true,
	"exec-timeout":  true,
	"issue-key":     true,
	"codex-bin":     true,
	"list-pages":    true,
}

// ParsedArgs is the result of scanning an argv. Claude flags are accepted
// and ignored; only --resume is consumed with meaning (design §2.1).
type ParsedArgs struct {
	// Resume is the session id from --resume, "" when absent. The daemon
	// feeds back the shim's own reported session_id (= cloud task id).
	Resume string
	// Shim maps each --shim-* flag (key = name after the prefix, e.g.
	// "env") to its raw string value. Duration/enum validation is the
	// caller's (F0 lives in run, not here).
	Shim map[string]string
	// Version and Help short-circuit: print and exit 0 touching nothing
	// (the daemon version-probes at registration; nonzero would
	// deregister the runtime).
	Version bool
	Help    bool
	// Warnings holds one entry per unknown flag, tolerated per C14:
	// --x=y is ignored whole; bare --x is treated as boolean and
	// consumes nothing (guessing arity could eat a real token).
	Warnings []string
}

// ParseArgs scans argv (without the program name). It never errors on
// unknown non-shim flags (C14); it errors on a malformed --shim-* flag —
// unknown name or missing value — which the caller maps to F0 E_CONFIG.
func ParseArgs(argv []string) (*ParsedArgs, error) {
	p := &ParsedArgs{Shim: map[string]string{}}
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		name, eqVal, hasEq := strings.Cut(tok, "=")
		switch {
		case tok == "--version":
			p.Version = true
		case tok == "--help":
			p.Help = true
		case strings.HasPrefix(name, "--shim-"):
			short := strings.TrimPrefix(name, "--shim-")
			if !shimFlagNames[short] {
				return nil, fmt.Errorf("unknown shim flag %q", name)
			}
			val := eqVal
			if !hasEq {
				if i+1 >= len(argv) {
					return nil, fmt.Errorf("shim flag %q requires a value", name)
				}
				i++
				val = argv[i]
			}
			p.Shim[short] = val
		case claudeValueFlags[name]:
			val := eqVal
			if !hasEq {
				if i+1 >= len(argv) {
					return nil, fmt.Errorf("flag %q requires a value", name)
				}
				i++
				val = argv[i]
			}
			if name == "--resume" {
				p.Resume = val
			}
			// All other claude value flags: accepted, ignored.
		case claudeBoolFlags[tok]:
			// Accepted, ignored.
		case strings.HasPrefix(tok, "-"):
			// Unknown flag (C14): =-form ignored whole, bare form
			// treated as boolean — never consume the next token.
			p.Warnings = append(p.Warnings, fmt.Sprintf("ignoring unknown flag %q", tok))
		default:
			// Positional: ignored — the prompt arrives on stdin
			// (design §2.1).
		}
	}
	return p, nil
}
