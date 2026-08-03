package run

import (
	"fmt"
	"strconv"
	"time"

	"github.com/mepuka/codex-cloud-shim/internal/protocol"
)

// Landing modes (design.md §2.2, fixed decision: default commit).
const (
	landReport = "report"
	landApply  = "apply"
	landCommit = "commit"
	landPush   = "push"
)

// settings are the validated shim flags plus defaults (design.md §2.2).
// protocol.ParseArgs returns raw strings; duration/enum validation is here —
// any malformed value is F0 E_CONFIG.
type settings struct {
	env      string // --shim-env; "" = derive from origin slug (§7.1)
	branch   string // --shim-branch; "" = detect (§7.2)
	landMode string
	frame    string // --shim-frame; cloud (default) wraps the submit prompt, off submits verbatim
	issueKey string // --shim-issue-key; "" = extract from prompt
	codexBin string
	resume   string // --resume session id (claude flag, consumed with meaning)

	attempts  int // exec --attempts passthrough
	attempt   int // apply --attempt passthrough
	listPages int

	pollInterval time.Duration
	keepalive    time.Duration
	deadline     time.Duration
	execTimeout  time.Duration
}

// newSettings applies defaults, the CODEX_CLOUD_SHIM_CODEX_BIN env fallback
// (flag wins — design.md C11), and validates every shim value.
func newSettings(args *protocol.ParsedArgs, getenv func(string) string) (*settings, error) {
	s := &settings{
		landMode:     landCommit,
		frame:        frameCloud,
		codexBin:     "codex",
		resume:       args.Resume,
		listPages:    5,
		pollInterval: 30 * time.Second,
		keepalive:    2 * time.Minute,
		deadline:     30 * time.Minute,
		execTimeout:  120 * time.Second,
	}
	if v := getenv("CODEX_CLOUD_SHIM_CODEX_BIN"); v != "" {
		s.codexBin = v
	}
	for name, val := range args.Shim {
		var err error
		switch name {
		case "env":
			s.env = val
		case "branch":
			s.branch = val
		case "land":
			switch val {
			case landReport, landApply, landCommit, landPush:
				s.landMode = val
			default:
				err = fmt.Errorf("must be one of report|apply|commit|push, got %q", val)
			}
		case "frame":
			switch val {
			case frameCloud, frameOff:
				s.frame = val
			default:
				err = fmt.Errorf("must be one of cloud|off, got %q", val)
			}
		case "issue-key":
			s.issueKey = val
		case "codex-bin":
			s.codexBin = val
		case "attempts":
			s.attempts, err = parsePositiveInt(val)
		case "attempt":
			s.attempt, err = parsePositiveInt(val)
		case "list-pages":
			s.listPages, err = parsePositiveInt(val)
		case "poll-interval":
			s.pollInterval, err = parsePositiveDuration(val)
		case "keepalive":
			s.keepalive, err = parsePositiveDuration(val)
		case "deadline":
			s.deadline, err = parsePositiveDuration(val)
		case "exec-timeout":
			s.execTimeout, err = parsePositiveDuration(val)
		default:
			// protocol.ParseArgs already rejects unknown shim names; this
			// guards against table drift between the two packages.
			err = fmt.Errorf("unhandled shim flag")
		}
		if err != nil {
			return nil, fmt.Errorf("--shim-%s %q: %w", name, val, err)
		}
	}
	return s, nil
}

func parsePositiveInt(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("not an integer: %w", err)
	}
	if n < 1 {
		return 0, fmt.Errorf("must be >= 1")
	}
	return n, nil
}

func parsePositiveDuration(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("not a duration: %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return d, nil
}
