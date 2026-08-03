package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// MaxStdinLineBytes caps the single prompt line (the daemon caps
	// events at 10 MB/line; the inbound line gets the same bound —
	// design §5.1).
	MaxStdinLineBytes = 10 << 20
	// MaxPromptBytes caps the extracted prompt text: it later becomes a
	// single argv element for `codex cloud exec`, so it must stay far
	// under ARG_MAX (design C12; overflow maps to F1).
	MaxPromptBytes = 100 << 10
)

// stdinLine is the one JSON line the daemon writes immediately after launch
// (buildClaudeInput, claude.go:739 — docs/multica-claude-contract.md
// §Invocation):
//
//	{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<prompt>"}]}}
//
// content is accepted as an array of text blocks or as a bare string
// (design §5.1).
type stdinLine struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// ReadPrompt reads exactly one line from r and extracts the prompt text.
// Timeouts and the background drainer are the caller's concern; every error
// returned here maps to F1 E_INPUT.
func ReadPrompt(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxStdinLineBytes)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				return "", fmt.Errorf("stdin prompt line exceeds %d bytes: %w", MaxStdinLineBytes, err)
			}
			return "", fmt.Errorf("read stdin prompt line: %w", err)
		}
		return "", fmt.Errorf("stdin closed before a prompt line arrived: %w", io.ErrUnexpectedEOF)
	}
	line := sc.Bytes()
	var msg stdinLine
	if err := json.Unmarshal(line, &msg); err != nil {
		return "", fmt.Errorf("parse stdin prompt line: %w", err)
	}
	text, err := extractText(msg.Message.Content)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", errors.New("stdin prompt line carries no text")
	}
	if len(text) > MaxPromptBytes {
		return "", fmt.Errorf("prompt is %d bytes, over the %d-byte limit", len(text), MaxPromptBytes)
	}
	return text, nil
}

// extractText accepts content as an array of text blocks (concatenated in
// order, joined with "\n"; non-text blocks skipped) or as a bare string.
func extractText(content json.RawMessage) (string, error) {
	if len(content) == 0 {
		return "", errors.New("stdin prompt line carries no message content")
	}
	var blocks []TextBlock
	if err := json.Unmarshal(content, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return joinLines(parts), nil
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s, nil
	}
	return "", errors.New("stdin prompt content is neither a text-block array nor a string")
}

func joinLines(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}
