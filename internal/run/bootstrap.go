package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mepuka/codex-cloud-shim/internal/gitctx"
)

// Multica does not pre-clone a checkout into the task workdir: the workdir
// carries context files and the AGENT runs `multica repo checkout <url>`
// itself, guided by the generated brief (measured 2026-08-03 — daemon source:
// task.Repos are "available for checkout", registerTaskRepos exists so
// "`multica repo checkout` would [not] reject project-only URLs"; execenv
// writes .multica/project/resources.json into the workdir "so agents can rely
// on its presence"). A real LLM agent reads the brief; this shim follows the
// same convention mechanically, from the machine-readable file, never the
// prose.

const (
	resourcesRelPath  = ".multica/project/resources.json"
	checkoutTimeout   = 5 * time.Minute
	multicaBinDefault = "multica"
)

// projectResourcesFile mirrors execenv's projectResourceFile
// (multica @ 8df7549, server/internal/daemon/execenv/execenv.go:29,
// context.go writeProjectResources).
type projectResourcesFile struct {
	ProjectID string `json:"project_id"`
	Resources []struct {
		ResourceType string          `json:"resource_type"`
		ResourceRef  json.RawMessage `json:"resource_ref"`
		Label        string          `json:"label,omitempty"`
	} `json:"resources"`
}

// githubRepoRef mirrors the server's validated github_repo payload
// (server/internal/handler/project_resource.go:84).
type githubRepoRef struct {
	URL string `json:"url"`
	Ref string `json:"ref,omitempty"`
}

// repoURLsFromWorkdir reads the workdir's project-resources sidecar and
// returns the github_repo URLs it declares. A missing file returns (nil, nil):
// the task simply carries no project context.
func repoURLsFromWorkdir(workdir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(resourcesRelPath)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", resourcesRelPath, err)
	}
	var f projectResourcesFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", resourcesRelPath, err)
	}
	var urls []string
	for _, r := range f.Resources {
		if r.ResourceType != "github_repo" {
			continue
		}
		var ref githubRepoRef
		if err := json.Unmarshal(r.ResourceRef, &ref); err != nil || ref.URL == "" {
			continue
		}
		urls = append(urls, ref.URL)
	}
	return urls, nil
}

// bootstrapCheckout materializes the task's repo checkout the way the
// platform expects the agent to: `multica repo checkout <url>` in the
// workdir (a worktree from the daemon's bare cache — no network). Exactly
// one github_repo resource is required; zero or several refuses with the
// candidates named, never a guess.
func (r *runner) bootstrapCheckout(ctx context.Context, workdir string) *failure {
	urls, err := repoURLsFromWorkdir(workdir)
	if err != nil {
		return failf(codeGitContext, "", "%v", err)
	}
	switch {
	case len(urls) == 0:
		return failf(codeGitContext, "",
			"no checkout in the workdir and %s declares no github_repo resource; attach the repo to the project and register it in the workspace (`multica repo add <url>`)",
			resourcesRelPath)
	case len(urls) > 1:
		return failf(codeGitContext, "",
			"no checkout in the workdir and %s declares %d github_repo resources (%s); ambiguous — refusing to guess",
			resourcesRelPath, len(urls), strings.Join(urls, ", "))
	}
	url := urls[0]
	r.log("info", fmt.Sprintf("no checkout in workdir; running `multica repo checkout %s` (platform convention)", url))

	cctx, cancel := context.WithTimeout(ctx, checkoutTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.multicaBin(), "repo", "checkout", url)
	cmd.Dir = workdir
	cmd.WaitDelay = 5 * time.Second
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return failf(codeGitContext, "", "multica repo checkout %s failed: %v\n%.500s", url, runErr, out)
	}
	return nil
}

func (r *runner) multicaBin() string {
	if v := r.cfg.Getenv("CODEX_CLOUD_SHIM_MULTICA_BIN"); v != "" {
		return v
	}
	return multicaBinDefault
}

// ensureWorktree runs discovery, falling back to the platform checkout
// convention exactly once when the workdir is empty of repos.
func (r *runner) ensureWorktree(ctx context.Context) *failure {
	wt, rule, err := gitctx.DiscoverWorktree(ctx, r.cfg.Worktree)
	if err == nil {
		if wt != r.cfg.Worktree {
			r.cfg.Worktree = wt
			r.log("info", fmt.Sprintf("worktree %s (rule: %s)", wt, rule))
		}
		return nil
	}
	if !errors.Is(err, gitctx.ErrNoCheckout) {
		return failf(codeGitContext, "", "%v", err)
	}
	if f := r.bootstrapCheckout(ctx, r.cfg.Worktree); f != nil {
		return f
	}
	wt, rule, err = gitctx.DiscoverWorktree(ctx, r.cfg.Worktree)
	if err != nil {
		return failf(codeGitContext, "", "after multica repo checkout: %v", err)
	}
	r.cfg.Worktree = wt
	r.log("info", fmt.Sprintf("worktree %s (rule: %s, after platform checkout)", wt, rule))
	return nil
}
