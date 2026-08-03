package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// The measured daemon shape (buildClaudeInput, claude.go:739 —
// docs/multica-claude-contract.md §Invocation).
const daemonPromptLine = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Create docs/PROBE.md with the current UTC date."}]}}` + "\n"

func TestReadPromptTable(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{
			name: "daemon shape single text block",
			in:   daemonPromptLine,
			want: "Create docs/PROBE.md with the current UTC date.",
		},
		{
			name: "multi-block content joined with newline",
			in:   `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}}` + "\n",
			want: "first\nsecond",
		},
		{
			name: "bare string content",
			in:   `{"type":"user","message":{"role":"user","content":"plain prompt"}}` + "\n",
			want: "plain prompt",
		},
		{
			name: "non-text blocks skipped",
			in:   `{"type":"user","message":{"role":"user","content":[{"type":"image","text":"nope"},{"type":"text","text":"kept"}]}}` + "\n",
			want: "kept",
		},
		{
			name: "no trailing newline still accepted",
			in:   `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"eof-terminated"}]}}`,
			want: "eof-terminated",
		},
		{
			name: "only the first line is consumed",
			in:   daemonPromptLine + `{"type":"user","message":{"role":"user","content":"second line"}}` + "\n",
			want: "Create docs/PROBE.md with the current UTC date.",
		},
		{
			name:    "empty text errors",
			in:      `{"type":"user","message":{"role":"user","content":[{"type":"text","text":""}]}}` + "\n",
			wantErr: "no text",
		},
		{
			name:    "empty content array errors",
			in:      `{"type":"user","message":{"role":"user","content":[]}}` + "\n",
			wantErr: "no text",
		},
		{
			name:    "missing content errors",
			in:      `{"type":"user","message":{"role":"user"}}` + "\n",
			wantErr: "no message content",
		},
		{
			name:    "malformed JSON errors",
			in:      `{"type":"user","message":` + "\n",
			wantErr: "parse stdin prompt line",
		},
		{
			name:    "content of wrong type errors",
			in:      `{"type":"user","message":{"role":"user","content":42}}` + "\n",
			wantErr: "neither a text-block array nor a string",
		},
		{
			name:    "immediate EOF errors",
			in:      "",
			wantErr: "before a prompt line",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadPrompt(strings.NewReader(tt.in))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got text %q", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadPrompt: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadPromptOversizePrompt(t *testing.T) {
	// One byte over the 100 KiB argv-element guard (design C12) → F1.
	big := strings.Repeat("a", MaxPromptBytes+1)
	line, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": big}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ReadPrompt(strings.NewReader(string(line) + "\n"))
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("want over-limit error, got %v", err)
	}
	// Exactly at the limit is accepted.
	exact := strings.Repeat("a", MaxPromptBytes)
	line, err = json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": exact},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadPrompt(strings.NewReader(string(line) + "\n"))
	if err != nil {
		t.Fatalf("at-limit prompt rejected: %v", err)
	}
	if len(got) != MaxPromptBytes {
		t.Errorf("got %d bytes, want %d", len(got), MaxPromptBytes)
	}
}

func TestReadPromptOversizeLine(t *testing.T) {
	// A line beyond the 10 MiB cap with no newline must error, not hang.
	in := strings.Repeat("x", MaxStdinLineBytes+16)
	_, err := ReadPrompt(strings.NewReader(in))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want line-too-long error, got %v", err)
	}
}
