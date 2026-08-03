// Package envcheck resolves whether a Codex Cloud environment exists for a
// repo, using the same undocumented backend surface the codex TUI's own
// autodetection reads (codex-rs cloud-tasks/src/env_detect.rs: GET
// {base}/wham/environments/by-repo/github/{owner}/{repo}) and the same local
// credential the CLI uses (~/.codex/auth.json; CODEX_HOME honored).
//
// DELIBERATELY OFF THE FIRE PATH. Environment creation has no API — it lives
// only in the chatgpt.com web UI — so this package exists for onboarding
// ergonomics (`codex-cloud-shim env check`): verify, resolve the id, and
// point at the create page when missing. If OpenAI changes the endpoint the
// command degrades to "unknown — use codex cloud list --env <slug>" and
// nothing else in the shim moves.
package envcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// CreateURL is where environments are created — the one step with no API.
const CreateURL = "https://chatgpt.com/codex/settings/environments"

const (
	defaultBaseURL = "https://chatgpt.com/backend-api"
	httpTimeout    = 30 * time.Second
)

// ErrNotLoggedIn means auth.json is missing or carries no ChatGPT tokens.
var ErrNotLoggedIn = errors.New("codex is not logged in with ChatGPT (run `codex login`)")

// Env is one environment row from the by-repo listing.
type Env struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	IsPinned  bool   `json:"is_pinned"`
	TaskCount int64  `json:"task_count"`
}

// Auth is the local codex ChatGPT credential, read exactly where the CLI
// keeps it. The token never leaves the machine except toward BaseURL.
type Auth struct {
	AccessToken string
	AccountID   string
}

type authFile struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

// LoadAuth reads $CODEX_HOME/auth.json (default ~/.codex/auth.json).
func LoadAuth(getenv func(string) string) (*Auth, error) {
	home := getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	b, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotLoggedIn
		}
		return nil, fmt.Errorf("read auth.json: %w", err)
	}
	var f authFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse auth.json: %w", err)
	}
	if f.Tokens.AccessToken == "" {
		return nil, ErrNotLoggedIn
	}
	return &Auth{AccessToken: f.Tokens.AccessToken, AccountID: f.Tokens.AccountID}, nil
}

// Client queries the environments surface.
type Client struct {
	Auth    *Auth
	BaseURL string       // "" = defaultBaseURL
	HTTP    *http.Client // nil = 30s-timeout default
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return defaultBaseURL
	}
	return c.BaseURL
}

func (c *Client) http() *http.Client {
	if c.HTTP == nil {
		return &http.Client{Timeout: httpTimeout}
	}
	return c.HTTP
}

// ByRepo lists the environments attached to github.com/owner/repo. An empty
// slice means "none — create one at CreateURL".
func (c *Client) ByRepo(ctx context.Context, owner, repo string) ([]Env, error) {
	url := fmt.Sprintf("%s/wham/environments/by-repo/github/%s/%s", c.base(), owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Auth.AccessToken)
	if c.Auth.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", c.Auth.AccountID)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("environments by-repo: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("environments by-repo: read: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w (HTTP %d)", ErrNotLoggedIn, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("environments by-repo: HTTP %d: %.200s", resp.StatusCode, body)
	}
	// The endpoint has been observed returning both a bare array and a
	// wrapped object; accept either (never guess which vintage is live).
	var list []Env
	if err := json.Unmarshal(body, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Environments []Env `json:"environments"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Environments != nil {
		return wrapped.Environments, nil
	}
	return nil, fmt.Errorf("environments by-repo: unrecognized response shape: %.200s", body)
}
