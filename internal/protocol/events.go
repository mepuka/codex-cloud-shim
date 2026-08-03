// Package protocol implements the claude stream-json wire protocol the
// Multica daemon consumes (docs/multica-claude-contract.md §Output) in the
// exact shapes of docs/design.md §4: event structs whose field order is the
// wire order, an Emitter that writes one JSON object per line to an
// io.Writer, the claude-compatible argv scanner (design §2), and the stdin
// prompt reader (design §5.1).
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

// TextBlock is one content block of an assistant message (or of the inbound
// user prompt, docs/multica-claude-contract.md §Invocation).
type TextBlock struct {
	Type string `json:"type"` // always "text"
	Text string `json:"text"`
}

// Message is the assistant-message payload (design §4.1).
type Message struct {
	Role    string      `json:"role"`  // "assistant"
	Model   string      `json:"model"` // "codex-cloud"
	Content []TextBlock `json:"content"`
}

// LogBody is the payload of a "log" event, forwarded verbatim by the daemon
// (docs/multica-claude-contract.md §Output, event table).
type LogBody struct {
	Level   string `json:"level"` // "info"|"warn"|"error"
	Message string `json:"message"`
}

// Event is the stream-json envelope. Field order IS wire order — golden
// tests are byte-stable against docs/design.md §4.2.
type Event struct {
	Type       string   `json:"type"`                  // "system"|"assistant"|"log"|"result"
	Subtype    string   `json:"subtype,omitempty"`     // system:"init"; result:"success"|"error_during_execution"
	SessionID  string   `json:"session_id,omitempty"`  // the Codex Cloud task id
	Message    *Message `json:"message,omitempty"`     // assistant only
	Log        *LogBody `json:"log,omitempty"`         // log only
	DurationMS int64    `json:"duration_ms,omitempty"` // result only
	NumTurns   int      `json:"num_turns,omitempty"`   // result only, always 1
	IsError    *bool    `json:"is_error,omitempty"`    // result only (pointer: false must serialize)
	Result     string   `json:"result,omitempty"`      // result only
}

const (
	// ModelName is the model reported on every assistant event.
	ModelName = "codex-cloud"

	// SubtypeInit is the system-event subtype the daemon requires early
	// (it captures session_id and flips status to "running").
	SubtypeInit = "init"
	// SubtypeSuccess is the success-result subtype.
	SubtypeSuccess = "success"
	// SubtypeError is the error-result subtype.
	SubtypeError = "error_during_execution"
)

// ErrAfterResult is returned by every Emitter method once a result event has
// been written: exactly one result, always last (design §4.1, result latch).
var ErrAfterResult = errors.New("protocol: result already emitted; emitter is latched")

// Emitter serializes events to w as NDJSON: one json.Marshal + "\n" per
// event, a single Write under a mutex (design §4.1). It carries no
// constructor for "user" or "control_request" events — the daemon treats
// those as agent-fatal or as a control handshake respectively
// (docs/multica-claude-contract.md §Output) and the shim must never emit
// them.
type Emitter struct {
	mu       sync.Mutex
	w        io.Writer
	resulted bool
}

// NewEmitter returns an Emitter writing to w (stdout in production).
func NewEmitter(w io.Writer) *Emitter {
	return &Emitter{w: w}
}

// ResultEmitted reports whether the result latch has closed. The CANCEL path
// uses it: stdin EOF after the result is the daemon's normal close and is
// ignored (design §5.1).
func (e *Emitter) ResultEmitted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resulted
}

func (e *Emitter) emit(ev *Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resulted {
		return ErrAfterResult
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("protocol: marshal %s event: %w", ev.Type, err)
	}
	b = append(b, '\n')
	if _, err := e.w.Write(b); err != nil {
		return fmt.Errorf("protocol: write %s event: %w", ev.Type, err)
	}
	if ev.Type == "result" {
		e.resulted = true
	}
	return nil
}

// SystemInit emits E1, the required-early system event carrying the task id
// (design §4.2 E1).
func (e *Emitter) SystemInit(sessionID string) error {
	return e.emit(&Event{Type: "system", Subtype: SubtypeInit, SessionID: sessionID})
}

// Assistant emits an assistant narration event (E2–E5). sessionID may be
// empty only before a task exists.
func (e *Emitter) Assistant(sessionID, text string) error {
	return e.emit(&Event{
		Type:      "assistant",
		SessionID: sessionID,
		Message: &Message{
			Role:    "assistant",
			Model:   ModelName,
			Content: []TextBlock{{Type: "text", Text: text}},
		},
	})
}

// Log emits E6, a diagnostics event the daemon forwards. level is one of
// "info"|"warn"|"error".
func (e *Emitter) Log(sessionID, level, message string) error {
	return e.emit(&Event{Type: "log", SessionID: sessionID, Log: &LogBody{Level: level, Message: message}})
}

// ResultSuccess emits E7 and closes the latch. num_turns is always 1.
func (e *Emitter) ResultSuccess(sessionID string, durationMS int64, result string) error {
	f := false
	return e.emit(&Event{
		Type:       "result",
		Subtype:    SubtypeSuccess,
		SessionID:  sessionID,
		DurationMS: durationMS,
		NumTurns:   1,
		IsError:    &f,
		Result:     result,
	})
}

// ResultError emits E8/E9 and closes the latch. Pass sessionID == "" when no
// task exists yet (pre-submit failures, and E9 where the pointer is being
// dropped) so no bogus id enters the daemon's resume pointer (design §4.2).
func (e *Emitter) ResultError(sessionID string, durationMS int64, result string) error {
	t := true
	return e.emit(&Event{
		Type:       "result",
		Subtype:    SubtypeError,
		SessionID:  sessionID,
		DurationMS: durationMS,
		NumTurns:   1,
		IsError:    &t,
		Result:     result,
	})
}

// RedactedPhrase replaces resume-reject phrase matches in quoted upstream
// text (design §4.1, phrase guard).
const RedactedPhrase = "[redacted-resume-phrase]"

// RedactPhrases replaces every occurrence of each phrase in text with
// RedactedPhrase. Callers apply it to all upstream stderr quoted into any
// event except the deliberate E9 template, so an incidental match can never
// fire the daemon's fresh-retry lever (docs/multica-claude-contract.md
// §Session identity).
func RedactPhrases(text string, phrases ...string) string {
	for _, p := range phrases {
		if p == "" {
			continue
		}
		text = strings.ReplaceAll(text, p, RedactedPhrase)
	}
	return text
}

// MaxEmbeddedBytes bounds the one large embeddable field (the report-mode
// diff): the daemon caps events at 10 MB/line, the design at 1 MiB for the
// diff (design §4.1).
const MaxEmbeddedBytes = 1 << 20

// TruncatedMarker is appended to any truncated embed.
const TruncatedMarker = "\n[truncated]"

// Truncate returns s unchanged when it fits in max bytes; otherwise it cuts
// s at max (backing off to a rune boundary) and appends TruncatedMarker.
func Truncate(s string, max int) string {
	if max <= 0 {
		if s == "" {
			return s
		}
		return TruncatedMarker
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + TruncatedMarker
}
