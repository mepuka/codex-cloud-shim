package protocol

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Golden-byte tests: the event shapes of docs/design.md §4.2, field order =
// wire order, one JSON object per line with a trailing newline.
func TestEmitterGoldenBytes(t *testing.T) {
	const sid = "task_e_6a70b48f64648323bba8af2747578941"
	tests := []struct {
		name string
		emit func(e *Emitter) error
		want string
	}{
		{
			name: "E1 system init",
			emit: func(e *Emitter) error { return e.SystemInit(sid) },
			want: `{"type":"system","subtype":"init","session_id":"task_e_6a70b48f64648323bba8af2747578941"}` + "\n",
		},
		{
			name: "E2 assistant submitted",
			emit: func(e *Emitter) error {
				return e.Assistant(sid, "Submitted Codex Cloud task "+sid+" (env mepuka/codex-cloud-shim, base branch main).\nhttps://chatgpt.com/codex/tasks/"+sid+"\nPolling every 30s; deadline 30m.")
			},
			want: `{"type":"assistant","session_id":"task_e_6a70b48f64648323bba8af2747578941","message":{"role":"assistant","model":"codex-cloud","content":[{"type":"text","text":"Submitted Codex Cloud task task_e_6a70b48f64648323bba8af2747578941 (env mepuka/codex-cloud-shim, base branch main).\nhttps://chatgpt.com/codex/tasks/task_e_6a70b48f64648323bba8af2747578941\nPolling every 30s; deadline 30m."}]}}` + "\n",
		},
		{
			name: "E3 assistant state change",
			emit: func(e *Emitter) error {
				return e.Assistant(sid, "Task "+sid+" status: ready (+3/-0, 1 file). Elapsed 1m50s.")
			},
			want: `{"type":"assistant","session_id":"task_e_6a70b48f64648323bba8af2747578941","message":{"role":"assistant","model":"codex-cloud","content":[{"type":"text","text":"Task task_e_6a70b48f64648323bba8af2747578941 status: ready (+3/-0, 1 file). Elapsed 1m50s."}]}}` + "\n",
		},
		{
			name: "E6 log with session id",
			emit: func(e *Emitter) error {
				return e.Log(sid, "info", "base branch main (rule: upstream of HEAD)")
			},
			want: `{"type":"log","session_id":"task_e_6a70b48f64648323bba8af2747578941","log":{"level":"info","message":"base branch main (rule: upstream of HEAD)"}}` + "\n",
		},
		{
			name: "E6 log before a task exists omits session_id",
			emit: func(e *Emitter) error { return e.Log("", "warn", "snapshot list failed") },
			want: `{"type":"log","log":{"level":"warn","message":"snapshot list failed"}}` + "\n",
		},
		{
			name: "E7 result success (is_error false serializes)",
			emit: func(e *Emitter) error {
				return e.ResultSuccess(sid, 123456, "Codex Cloud task "+sid+" completed.")
			},
			want: `{"type":"result","subtype":"success","session_id":"task_e_6a70b48f64648323bba8af2747578941","duration_ms":123456,"num_turns":1,"is_error":false,"result":"Codex Cloud task task_e_6a70b48f64648323bba8af2747578941 completed."}` + "\n",
		},
		{
			name: "E8 result error with session id",
			emit: func(e *Emitter) error {
				return e.ResultError(sid, 8000, "deadline 30m0s exceeded; task last status \"pending\"\nThe upstream Codex Cloud run was NOT cancelled and may still complete; a retry will reconcile by task id.")
			},
			want: `{"type":"result","subtype":"error_during_execution","session_id":"task_e_6a70b48f64648323bba8af2747578941","duration_ms":8000,"num_turns":1,"is_error":true,"result":"deadline 30m0s exceeded; task last status \"pending\"\nThe upstream Codex Cloud run was NOT cancelled and may still complete; a retry will reconcile by task id."}` + "\n",
		},
		{
			name: "E8 pre-submit error omits session_id",
			emit: func(e *Emitter) error {
				return e.ResultError("", 100, "codex binary not found on PATH")
			},
			want: `{"type":"result","subtype":"error_during_execution","duration_ms":100,"num_turns":1,"is_error":true,"result":"codex binary not found on PATH"}` + "\n",
		},
		{
			name: "E9 resume-reject omits session_id",
			emit: func(e *Emitter) error {
				return e.ResultError("", 2500, "No conversation found with session ID: "+sid+". The Codex Cloud task id could not be found upstream; start a fresh session.")
			},
			want: `{"type":"result","subtype":"error_during_execution","duration_ms":2500,"num_turns":1,"is_error":true,"result":"No conversation found with session ID: task_e_6a70b48f64648323bba8af2747578941. The Codex Cloud task id could not be found upstream; start a fresh session."}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := NewEmitter(&buf)
			if err := tt.emit(e); err != nil {
				t.Fatalf("emit: %v", err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("wire bytes mismatch\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestEmitterOneEventPerLine(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	const sid = "task_e_0"
	if err := e.SystemInit(sid); err != nil {
		t.Fatal(err)
	}
	if err := e.Assistant(sid, "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	if err := e.ResultSuccess(sid, 1, "done"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("output does not end in newline: %q", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 physical lines (embedded \\n must stay escaped), got %d: %q", len(lines), out)
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, "{") || !strings.HasSuffix(l, "}") {
			t.Errorf("line %d is not a single JSON object: %q", i, l)
		}
	}
}

func TestEmitterResultLatch(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	if e.ResultEmitted() {
		t.Fatal("latch closed before any result")
	}
	if err := e.ResultSuccess("task_e_1", 5, "ok"); err != nil {
		t.Fatal(err)
	}
	if !e.ResultEmitted() {
		t.Fatal("latch open after result")
	}
	before := buf.Len()
	for name, err := range map[string]error{
		"SystemInit":  e.SystemInit("task_e_1"),
		"Assistant":   e.Assistant("task_e_1", "late"),
		"Log":         e.Log("task_e_1", "info", "late"),
		"ResultError": e.ResultError("task_e_1", 1, "late"),
		"ResultOK":    e.ResultSuccess("task_e_1", 1, "late"),
	} {
		if !errors.Is(err, ErrAfterResult) {
			t.Errorf("%s after result: got %v, want ErrAfterResult", name, err)
		}
	}
	if buf.Len() != before {
		t.Errorf("bytes written after the result latch closed: %q", buf.String()[before:])
	}
}

type failWriter struct{ err error }

func (f failWriter) Write(p []byte) (int, error) { return 0, f.err }

func TestEmitterWriteFailureDoesNotLatch(t *testing.T) {
	e := NewEmitter(failWriter{err: errors.New("broken pipe")})
	if err := e.ResultSuccess("task_e_1", 1, "ok"); err == nil {
		t.Fatal("want write error")
	}
	if e.ResultEmitted() {
		t.Fatal("failed write must not close the latch (no result reached the daemon)")
	}
}

// The emitter must expose no way to build the two forbidden event types
// (design §4.1): "user" (async_launched inside one fails the whole run) and
// "control_request" (triggers daemon control_response writes).
func TestEmitterHasNoForbiddenConstructors(t *testing.T) {
	typ := reflect.TypeOf(&Emitter{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		lower := strings.ToLower(name)
		if strings.Contains(lower, "user") || strings.Contains(lower, "control") {
			t.Errorf("forbidden constructor on Emitter: %s", name)
		}
	}
}

func TestRedactPhrases(t *testing.T) {
	const phrase = "No conversation found with session ID"
	tests := []struct {
		name, in, want string
	}{
		{
			name: "phrase inside quoted stderr",
			in:   "upstream said: No conversation found with session ID: task_e_9",
			want: "upstream said: [redacted-resume-phrase]: task_e_9",
		},
		{
			name: "two occurrences",
			in:   phrase + " and again " + phrase,
			want: "[redacted-resume-phrase] and again [redacted-resume-phrase]",
		},
		{name: "no match untouched", in: "environment 'x' not found", want: "environment 'x' not found"},
		{name: "empty phrase list untouched", in: "anything", want: "anything"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if tt.name == "empty phrase list untouched" {
				got = RedactPhrases(tt.in)
			} else {
				got = RedactPhrases(tt.in, phrase)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 100); got != "short" {
		t.Errorf("under limit changed: %q", got)
	}
	if got := Truncate("abcdef", 3); got != "abc"+TruncatedMarker {
		t.Errorf("truncation: got %q", got)
	}
	// Never cut inside a multi-byte rune.
	in := "aé" // 'é' is 2 bytes starting at index 1
	if got := Truncate(in, 2); got != "a"+TruncatedMarker {
		t.Errorf("rune boundary: got %q", got)
	}
	if got := Truncate("", 0); got != "" {
		t.Errorf("empty at zero max: got %q", got)
	}
	if MaxEmbeddedBytes != 1<<20 {
		t.Errorf("MaxEmbeddedBytes = %d, want 1 MiB (design §4.1)", MaxEmbeddedBytes)
	}
}
