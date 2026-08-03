// Package state persists the reconcile marker — the cross-process memory
// that makes retries adopt an already-submitted cloud task instead of
// double-submitting (design.md §5.3, C2). The marker lives inside the git
// dir (`<git-dir>/codex-cloud-shim/state.json`) so it survives shim death
// and is never part of the delivered tree.
package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Marker records one submitted cloud task (design.md §5.3).
type Marker struct {
	TaskID       string `json:"task_id"`
	URL          string `json:"url"`
	PromptSHA256 string `json:"prompt_sha256"`
	Env          string `json:"env"`
	Branch       string `json:"branch"`
	Land         string `json:"land"`
	SubmittedAt  string `json:"submitted_at"`
}

// PromptHash is the marker's prompt identity: hex SHA-256 of the exact
// prompt text. Same hash on a later launch = retry; different = follow-up
// (design.md §5.4).
func PromptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// Path resolves the marker location under the worktree's git dir.
func Path(ctx context.Context, worktree string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = worktree
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git rev-parse --git-dir: %w (%s)", err, strings.TrimSpace(errBuf.String()))
	}
	gitDir := strings.TrimSpace(out.String())
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktree, gitDir)
	}
	return filepath.Join(gitDir, "codex-cloud-shim", "state.json"), nil
}

// Load reads the marker. A missing marker is (nil, nil); a corrupt marker is
// an error the caller downgrades to a warning and treats as no marker
// (design.md §5.3).
func Load(ctx context.Context, worktree string) (*Marker, error) {
	p, err := Path(ctx, worktree)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read marker: %w", err)
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("corrupt marker %s: %w", p, err)
	}
	if m.TaskID == "" {
		return nil, fmt.Errorf("corrupt marker %s: empty task_id", p)
	}
	return &m, nil
}

// Save writes the marker durably (temp file, fsync, rename) — it must
// survive the crash window between exec parsing and E1 (design.md §5.3).
func Save(ctx context.Context, worktree string, m *Marker) error {
	p, err := Path(ctx, worktree)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("marker dir: %w", err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "state-*.json")
	if err != nil {
		return fmt.Errorf("marker temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write marker: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close marker: %w", err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("rename marker into place: %w", err)
	}
	return nil
}

// Delete removes the marker (dead-task reconcile, design.md §5.3). Missing
// is not an error.
func Delete(ctx context.Context, worktree string) error {
	p, err := Path(ctx, worktree)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete marker: %w", err)
	}
	return nil
}
