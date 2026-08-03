package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mepuka/codex-cloud-shim/internal/protocol"
)

func TestSweepStaleScratchRemovesOnlyOldPrefixed(t *testing.T) {
	tmp := t.TempDir()
	now := time.Now()

	old := filepath.Join(tmp, scratchPrefix+"old")
	fresh := filepath.Join(tmp, scratchPrefix+"fresh")
	foreign := filepath.Join(tmp, "unrelated-dir")
	for _, d := range []string{old, fresh, foreign} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stale := now.Add(-scratchMaxAge - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreign, stale, stale); err != nil {
		t.Fatal(err)
	}

	sweepStaleScratch(tmp, now)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("stale prefixed dir survived the sweep: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh prefixed dir was swept: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign dir was swept: %v", err)
	}
}

// failingWriter errors on every write after the first n bytes-writes succeed.
type failingWriter struct{ writesLeft int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.writesLeft > 0 {
		w.writesLeft--
		return len(p), nil
	}
	return 0, errors.New("broken pipe")
}

func TestNoteEmitCancelsOnWriteFailureOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	em := protocol.NewEmitter(&failingWriter{writesLeft: 1})
	r := &runner{em: em, cancelRun: cancel}

	r.log("info", "first write succeeds") // consumes the working write
	if ctx.Err() != nil {
		t.Fatal("successful emit must not cancel")
	}
	before := r.lastEmit

	r.say("second write fails")
	if ctx.Err() == nil {
		t.Fatal("failed event write must cancel the run")
	}
	if r.lastEmit != before {
		t.Error("failed write must not count as an emit for the keepalive window")
	}
}

func TestNoteEmitIgnoresResultLatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	em := protocol.NewEmitter(&failingWriter{writesLeft: 1})
	if err := em.ResultSuccess("sid", 1, "done"); err != nil {
		t.Fatal(err)
	}
	r := &runner{em: em, cancelRun: cancel}
	r.say("late keepalive tick after the result")
	if ctx.Err() != nil {
		t.Fatal("ErrAfterResult is the latch working, not a write failure; it must not cancel")
	}
}
