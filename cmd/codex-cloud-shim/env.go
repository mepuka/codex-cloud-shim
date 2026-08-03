package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mepuka/codex-cloud-shim/internal/envcheck"
	"github.com/mepuka/codex-cloud-shim/internal/gitctx"
)

const envUsage = `codex-cloud-shim env check [owner/repo]

Verifies a Codex Cloud environment exists for the repo (default: the CWD's
origin slug) using the local codex login. Environments are created only in
the web UI — when none exists this prints the create link. Exit 0 = exists,
1 = missing or unknown.
`

// envCommand is the operator-facing onboarding helper: creation has no API,
// so the best automatable step is a definitive check plus the exact pointer.
func envCommand(args []string) int {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(envUsage)
		return 0
	}
	if len(args) == 0 || args[0] != "check" {
		fmt.Print(envUsage)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	slug := ""
	if len(args) > 1 {
		slug = args[1]
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot determine CWD: %v\n", err)
			return 1
		}
		s, err := gitctx.OriginSlug(ctx, cwd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot derive repo slug from CWD origin (%v); pass owner/repo explicitly\n", err)
			return 1
		}
		slug = s
	}
	owner, repo, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || repo == "" {
		fmt.Fprintf(os.Stderr, "not an owner/repo slug: %q\n", slug)
		return 1
	}

	auth, err := envcheck.LoadAuth(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	envs, err := (&envcheck.Client{Auth: auth}).ByRepo(ctx, owner, repo)
	if err != nil {
		if errors.Is(err, envcheck.ErrNotLoggedIn) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		// Undocumented surface — degrade to the documented probe, never guess.
		fmt.Fprintf(os.Stderr, "environment lookup unavailable (%v)\nfall back to: codex cloud list --env %s --limit 1 --json\n", err, slug)
		return 1
	}
	if len(envs) == 0 {
		fmt.Printf("no Codex Cloud environment for %s\ncreate one (defaults are fine): %s\nthen re-run: codex-cloud-shim env check %s\n",
			slug, envcheck.CreateURL, slug)
		return 1
	}
	fmt.Printf("environment(s) for %s:\n", slug)
	for _, e := range envs {
		label := e.Label
		if label == "" {
			label = "(unlabeled)"
		}
		fmt.Printf("  %s  %s  (tasks: %d)\n", e.ID, label, e.TaskCount)
	}
	return 0
}
