package envcheck

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeAuth(t *testing.T, dir, content string) func(string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(k string) string {
		if k == "CODEX_HOME" {
			return dir
		}
		return ""
	}
}

func TestLoadAuthReadsCodexHome(t *testing.T) {
	getenv := writeAuth(t, t.TempDir(),
		`{"tokens":{"access_token":"tok-abc","account_id":"acct-1"}}`)
	a, err := LoadAuth(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if a.AccessToken != "tok-abc" || a.AccountID != "acct-1" {
		t.Fatalf("wrong auth: %+v", a)
	}
}

func TestLoadAuthMissingFileIsNotLoggedIn(t *testing.T) {
	dir := t.TempDir()
	getenv := func(k string) string {
		if k == "CODEX_HOME" {
			return dir
		}
		return ""
	}
	if _, err := LoadAuth(getenv); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("want ErrNotLoggedIn, got %v", err)
	}
}

func TestLoadAuthEmptyTokenIsNotLoggedIn(t *testing.T) {
	getenv := writeAuth(t, t.TempDir(), `{"tokens":{"access_token":""}}`)
	if _, err := LoadAuth(getenv); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("want ErrNotLoggedIn, got %v", err)
	}
}

func TestByRepoParsesBothShapesAndSendsHeaders(t *testing.T) {
	for name, body := range map[string]any{
		"bare-array": []Env{{ID: "env_1", Label: "o/r"}},
		"wrapped":    map[string]any{"environments": []Env{{ID: "env_1", Label: "o/r"}}},
	} {
		t.Run(name, func(t *testing.T) {
			var gotAuth, gotAcct, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				gotAcct = r.Header.Get("ChatGPT-Account-Id")
				gotPath = r.URL.Path
				json.NewEncoder(w).Encode(body)
			}))
			defer srv.Close()

			c := &Client{Auth: &Auth{AccessToken: "tok", AccountID: "acct"}, BaseURL: srv.URL}
			envs, err := c.ByRepo(context.Background(), "o", "r")
			if err != nil {
				t.Fatal(err)
			}
			if len(envs) != 1 || envs[0].ID != "env_1" {
				t.Fatalf("wrong envs: %+v", envs)
			}
			if gotAuth != "Bearer tok" || gotAcct != "acct" {
				t.Fatalf("wrong headers: %q %q", gotAuth, gotAcct)
			}
			if gotPath != "/wham/environments/by-repo/github/o/r" {
				t.Fatalf("wrong path: %q", gotPath)
			}
		})
	}
}

func TestByRepoUnauthorizedIsNotLoggedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{Auth: &Auth{AccessToken: "expired"}, BaseURL: srv.URL}
	if _, err := c.ByRepo(context.Background(), "o", "r"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("want ErrNotLoggedIn, got %v", err)
	}
}

func TestByRepoEmptyListMeansCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := &Client{Auth: &Auth{AccessToken: "tok"}, BaseURL: srv.URL}
	envs, err := c.ByRepo(context.Background(), "o", "r")
	if err != nil || len(envs) != 0 {
		t.Fatalf("want empty list, got %v %v", envs, err)
	}
}

func TestByRepoGarbageShapeErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"totally":"different"}`))
	}))
	defer srv.Close()
	c := &Client{Auth: &Auth{AccessToken: "tok"}, BaseURL: srv.URL}
	if _, err := c.ByRepo(context.Background(), "o", "r"); err == nil {
		t.Fatal("unrecognized shape must error, never guess")
	}
}
