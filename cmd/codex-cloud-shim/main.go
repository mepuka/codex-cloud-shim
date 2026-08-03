// codex-cloud-shim is a claude-CLI-compatible binary that fronts Codex Cloud
// for the Multica daemon: it accepts the daemon's claude argv and stream-json
// stdin, submits the prompt as a Codex Cloud task, polls it, lands the diff,
// and speaks the claude stream-json event protocol back (docs/design.md).
// This file is wiring only: argv → config, signal watcher, os.Exit.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mepuka/codex-cloud-shim/internal/protocol"
	"github.com/mepuka/codex-cloud-shim/internal/run"
)

const version = "codex-cloud-shim v0.1.0"

const usage = `codex-cloud-shim — Codex Cloud runner speaking the claude stream-json protocol

Claude-compatible flags are accepted and ignored (only --resume is consumed).
The prompt arrives as one stream-json line on stdin.

Shim flags (also accepted as --shim-x=v):
  --shim-env <label>           Codex Cloud environment (default: origin owner/repo slug)
  --shim-branch <b>            base branch (default: detected; --branch is always passed)
  --shim-land <mode>           report|apply|commit|push (default commit)
  --shim-attempts <N>          forwarded as exec --attempts N
  --shim-attempt <N>           forwarded as apply --attempt N
  --shim-poll-interval <dur>   list-poll cadence (default 30s)
  --shim-keepalive <dur>       max event silence while polling (default 2m)
  --shim-deadline <dur>        overall wall-clock budget (default 30m)
  --shim-exec-timeout <dur>    budget for the exec subprocess (default 120s)
  --shim-issue-key <KEY>       commit-title suffix (default: extracted from prompt)
  --shim-codex-bin <path>      codex executable (env CODEX_CLOUD_SHIM_CODEX_BIN)
  --shim-list-pages <N>        max list cursor pages (default 5)
`

func main() {
	os.Exit(realMain())
}

func realMain() int {
	args, err := protocol.ParseArgs(os.Args[1:])
	if err != nil {
		// F0 before anything else could happen: emit the error result on
		// the event stream — the daemon reads events, not stderr.
		em := protocol.NewEmitter(os.Stdout)
		em.ResultError("", 0, "E_CONFIG: "+err.Error())
		return run.ExitError
	}
	// --version/--help short-circuit, exit 0 touching nothing: the daemon
	// version-probes at registration and nonzero would deregister the
	// runtime (design.md §2.1).
	if args.Version {
		fmt.Println(version)
		return 0
	}
	if args.Help {
		fmt.Print(usage)
		return 0
	}

	worktree, err := os.Getwd()
	if err != nil {
		em := protocol.NewEmitter(os.Stdout)
		em.ResultError("", 0, "E_CONFIG: cannot determine working directory: "+err.Error())
		return run.ExitError
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SIGTERM/SIGINT → CANCEL: cancel local children (never the upstream
	// cloud run — it lives server-side) and guarantee exit within 2 s even
	// if a child ignores the kill (design.md §5.1/§8).
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-sigCh
		cancel()
		time.AfterFunc(2*time.Second, func() { os.Exit(run.ExitCancel) })
	}()

	return run.Run(ctx, run.Config{
		Args:     args,
		Stdin:    os.Stdin,
		Emitter:  protocol.NewEmitter(os.Stdout),
		Worktree: worktree,
		Getenv:   os.Getenv,
	})
}
