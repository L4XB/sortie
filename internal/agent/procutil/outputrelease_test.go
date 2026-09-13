package procutil

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer guarded by a mutex, for a logger the
// release's own background goroutine writes to concurrently with the
// test goroutine reading it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// warnSignalHandler wraps a slog.Handler and closes its signal channel
// once a record whose message equals msg has been handled, so a test
// waits for that specific record to have actually reached the buffer
// instead of racing it with a sleep.
type warnSignalHandler struct {
	slog.Handler
	msg    string
	once   sync.Once
	signal chan struct{}
}

func newWarnSignalHandler(w *syncBuffer, msg string) (*warnSignalHandler, <-chan struct{}) {
	signal := make(chan struct{})
	return &warnSignalHandler{
		Handler: slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}),
		msg:     msg,
		signal:  signal,
	}, signal
}

func (h *warnSignalHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.Handler.Handle(ctx, r)
	if r.Message == h.msg {
		h.once.Do(func() { close(h.signal) })
	}
	return err
}

// newTestOwnedPipes returns an *OwnedPipes backed by real pipe files,
// closed in cleanup. StartOutputRelease requires a non-nil Pipes, and
// the give-up arm calls CloseStdout on it, so the tests below need a
// real file rather than a nil stand-in.
func newTestOwnedPipes(t *testing.T) *OwnedPipes {
	t.Helper()
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		_ = outWrite.Close()
		_ = outRead.Close()
		_ = errWrite.Close()
		_ = errRead.Close()
	})
	return &OwnedPipes{Stdout: outRead, Stderr: errRead}
}

// TestStartOutputRelease_GraceElapsedAbandonsWithinBound asserts that,
// with the reap signalled and the standard-output write end still held (the
// hand-made ReaderDone channel is never closed), the release abandons
// within a bound derived from the injected grace, and the abandonment
// latch is set.
func TestStartOutputRelease_GraceElapsedAbandonsWithinBound(t *testing.T) {
	t.Parallel()

	const grace = 80 * time.Millisecond
	reaped := make(chan struct{})
	close(reaped)

	start := time.Now()
	r := StartOutputRelease(OutputReleaseParams{
		Pipes:      newTestOwnedPipes(t),
		Reaped:     reaped,
		ReaderDone: make(chan struct{}),
		Grace:      grace,
		Logger:     slog.New(slog.DiscardHandler),
	})

	select {
	case <-r.Abandoned():
	case <-time.After(2 * time.Second):
		t.Fatal("the release never abandoned within a bound well past the injected grace")
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Errorf("the release abandoned after %v, want at least the injected grace %v", elapsed, grace)
	}
	if !r.latch.Load() {
		t.Error("the release's latch is unset after abandonment, want it set")
	}
}

// TestStartOutputRelease_OnAbandonObservesGiveUpEffectsAlreadyApplied
// asserts that, by the time the give-up arm calls OnAbandon, the latch
// is already set, Abandoned() is already closed, and the standard-
// output read end is already closed (a read on it returns at once
// instead of blocking).
func TestStartOutputRelease_OnAbandonObservesGiveUpEffectsAlreadyApplied(t *testing.T) {
	t.Parallel()

	const grace = 60 * time.Millisecond
	reaped := make(chan struct{})
	pipes := newTestOwnedPipes(t)

	type observed struct {
		latchSet        bool
		abandonedClosed bool
		readErr         error
	}
	factsCh := make(chan observed, 1)

	var r *OutputRelease
	r = StartOutputRelease(OutputReleaseParams{
		Pipes:      pipes,
		Reaped:     reaped,
		ReaderDone: make(chan struct{}),
		Grace:      grace,
		OnAbandon: func() {
			var got observed
			got.latchSet = r.latch.Load()
			select {
			case <-r.Abandoned():
				got.abandonedClosed = true
			default:
			}
			buf := make([]byte, 1)
			_, got.readErr = pipes.Stdout.Read(buf)
			factsCh <- got
		},
		Logger: slog.New(slog.DiscardHandler),
	})
	// r must already hold the value assigned above before the release's
	// own goroutine can observe Reaped closed and read it inside
	// OnAbandon, or that read races the assignment.
	close(reaped)

	select {
	case got := <-factsCh:
		if !got.latchSet {
			t.Error("OnAbandon observed the latch unset, want it already stored")
		}
		if !got.abandonedClosed {
			t.Error("OnAbandon observed Abandoned() not yet closed, want it already closed")
		}
		if !errors.Is(got.readErr, os.ErrClosed) {
			t.Errorf("OnAbandon observed a read on the standard-output end returning %v, want %v (the read end must already be closed)", got.readErr, os.ErrClosed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnAbandon never ran")
	}
}

// TestOutputRelease_TurnEndMessage asserts that TurnEndMessage returns the
// fallback before abandonment and OutputAbandonedMessage once the
// release has given up.
func TestOutputRelease_TurnEndMessage(t *testing.T) {
	t.Parallel()

	const grace = 60 * time.Millisecond
	reaped := make(chan struct{})
	r := StartOutputRelease(OutputReleaseParams{
		Pipes:      newTestOwnedPipes(t),
		Reaped:     reaped,
		ReaderDone: make(chan struct{}),
		Grace:      grace,
		Logger:     slog.New(slog.DiscardHandler),
	})

	if got := r.TurnEndMessage("fallback"); got != "fallback" {
		t.Errorf("TurnEndMessage() before abandonment = %q, want the fallback unchanged", got)
	}

	close(reaped)
	select {
	case <-r.Abandoned():
	case <-time.After(2 * time.Second):
		t.Fatal("the release never abandoned")
	}

	if got := r.TurnEndMessage("fallback"); got != OutputAbandonedMessage {
		t.Errorf("TurnEndMessage() after abandonment = %q, want %q", got, OutputAbandonedMessage)
	}
}

// TestStartOutputRelease_ReaderDoneInsideGraceLeavesLatchUnset asserts
// that a ReaderDone close inside Grace leaves the latch unset, closes no
// read end, and emits no WARN.
func TestStartOutputRelease_ReaderDoneInsideGraceLeavesLatchUnset(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	const grace = 300 * time.Millisecond
	reaped := make(chan struct{})
	close(reaped)
	readerDone := make(chan struct{})

	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		_ = outWrite.Close()
		_ = outRead.Close()
		_ = errWrite.Close()
		_ = errRead.Close()
	})

	r := StartOutputRelease(OutputReleaseParams{
		Pipes:      &OwnedPipes{Stdout: outRead, Stderr: errRead},
		Reaped:     reaped,
		ReaderDone: readerDone,
		Grace:      grace,
		Logger:     logger,
	})

	close(readerDone)

	// The grace has definitely elapsed by this point, well past when the
	// give-up arm would have fired had ReaderDone not stopped it: a
	// bounded wait past the grace proves absence rather than assuming it
	// from a race window.
	<-time.After(grace + 400*time.Millisecond)

	if r.latch.Load() {
		t.Error("the release's latch is set though the reader ended inside its grace, want it unset")
	}
	if strings.Contains(buf.String(), OutputAbandonedWarning) {
		t.Errorf("the release logged %q though the reader ended inside its grace, want no WARN", OutputAbandonedWarning)
	}

	readDone := make(chan error, 1)
	go func() {
		b := make([]byte, 1)
		_, err := outRead.Read(b)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		t.Errorf("reading the standard-output read end returned %v, want it still open and blocked (the release must not have closed it)", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStartOutputRelease_GiveUpArmLogsExactlyOneWarnRecord asserts that
// the give-up arm emits exactly one record, at WARN, carrying
// OutputAbandonedWarning and a drain_bound attribute with the resolved
// grace.
func TestStartOutputRelease_GiveUpArmLogsExactlyOneWarnRecord(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	handler, warned := newWarnSignalHandler(&buf, OutputAbandonedWarning)
	logger := slog.New(handler)

	const grace = 60 * time.Millisecond
	reaped := make(chan struct{})
	close(reaped)

	StartOutputRelease(OutputReleaseParams{
		Pipes:      newTestOwnedPipes(t),
		Reaped:     reaped,
		ReaderDone: make(chan struct{}),
		Grace:      grace,
		Logger:     logger,
	})

	select {
	case <-warned:
	case <-time.After(2 * time.Second):
		t.Fatal("the give-up arm never logged its WARN record")
	}

	// A second record logged right after the give-up arm's own would
	// land well within this bound: the arm logs synchronously and its
	// goroutine returns immediately after, so waiting this long before
	// counting proves the record's absence rather than merely not yet
	// having observed it.
	<-time.After(300 * time.Millisecond)

	output := buf.String()
	wantRecord := "level=WARN msg=\"" + OutputAbandonedWarning + "\""
	if got := strings.Count(output, wantRecord); got != 1 {
		t.Errorf("give-up arm WARN record count = %d, want exactly 1 record %q: %q", got, wantRecord, output)
	}
	if !strings.Contains(output, "drain_bound="+grace.String()) {
		t.Errorf("WARN record %q does not carry drain_bound=%s", output, grace)
	}
}

// TestStartOutputRelease_StderrArmLogsItsOwnSeparateAbandonmentRecord
// asserts that the standard-error arm's own abandonment
// record is separate from the give-up arm's, with its own text, and the
// give-up arm's own record count is unaffected by it running.
func TestStartOutputRelease_StderrArmLogsItsOwnSeparateAbandonmentRecord(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	handler, warned := newWarnSignalHandler(&buf, OutputAbandonedWarning)
	logger := slog.New(handler)

	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		_ = stderrWrite.Close()
		_ = stderrRead.Close()
	})
	collector := NewStderrCollector(stderrRead, logger)

	const grace = 60 * time.Millisecond
	reaped := make(chan struct{})
	close(reaped)

	StartOutputRelease(OutputReleaseParams{
		Pipes:      newTestOwnedPipes(t),
		Reaped:     reaped,
		ReaderDone: make(chan struct{}),
		Stderr:     collector,
		Grace:      grace,
		Logger:     logger,
	})

	select {
	case <-warned:
	case <-time.After(2 * time.Second):
		t.Fatal("the give-up arm never logged its own WARN record")
	}
	select {
	case <-collector.abandoned:
	case <-time.After(2 * time.Second):
		t.Fatal("the standard-error collector was never abandoned")
	}

	// A second give-up-arm record logged right after its own would land
	// well within this bound, the same margin
	// TestStartOutputRelease_GiveUpArmLogsExactlyOneWarnRecord waits out,
	// so waiting this long before counting proves the record's absence.
	<-time.After(300 * time.Millisecond)

	output := buf.String()
	wantRecord := "level=WARN msg=\"" + OutputAbandonedWarning + "\""
	if got := strings.Count(output, wantRecord); got != 1 {
		t.Errorf("give-up arm WARN record count = %d, want exactly 1 record %q even with a Stderr arm running: %q", got, wantRecord, output)
	}
	if !strings.Contains(output, "level=WARN msg=\"agent stderr was not fully collected before the process was reaped\"") {
		t.Errorf("the standard-error arm's own separate abandonment WARN record is missing: %q", output)
	}
}

// TestOutputRelease_NilReceiver asserts that a nil *OutputRelease returns
// a nil channel from Abandoned and passes fallback through unchanged
// from TurnEndMessage.
func TestOutputRelease_NilReceiver(t *testing.T) {
	t.Parallel()

	var r *OutputRelease

	if ch := r.Abandoned(); ch != nil {
		t.Errorf("Abandoned() on a nil receiver = %v, want nil", ch)
	}
	if got := r.TurnEndMessage("fallback"); got != "fallback" {
		t.Errorf("TurnEndMessage() on a nil receiver = %q, want the fallback unchanged", got)
	}
}

// TestStartOutputRelease_PanicsOnNilRequiredFields covers the three
// required OutputReleaseParams fields: StartOutputRelease panics when
// any one of Pipes, Reaped, or ReaderDone is nil.
func TestStartOutputRelease_PanicsOnNilRequiredFields(t *testing.T) {
	t.Parallel()

	validPipes := newTestOwnedPipes(t)
	validReaped := make(chan struct{})
	validReaderDone := make(chan struct{})

	tests := []struct {
		name   string
		params OutputReleaseParams
	}{
		{
			name:   "nil Pipes",
			params: OutputReleaseParams{Reaped: validReaped, ReaderDone: validReaderDone},
		},
		{
			name:   "nil Reaped",
			params: OutputReleaseParams{Pipes: validPipes, ReaderDone: validReaderDone},
		},
		{
			name:   "nil ReaderDone",
			params: OutputReleaseParams{Pipes: validPipes, Reaped: validReaped},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				if recover() == nil {
					t.Fatal("StartOutputRelease() did not panic, want a panic on the missing required field")
				}
			}()
			StartOutputRelease(tt.params)
		})
	}
}

// TestStartOutputRelease_NonPositiveGraceResolvesToDefaultDrainGrace
// covers the non-positive Grace fallback: a zero Grace resolves to
// DefaultDrainGrace rather than firing immediately.
func TestStartOutputRelease_NonPositiveGraceResolvesToDefaultDrainGrace(t *testing.T) {
	t.Parallel()

	reaped := make(chan struct{})
	close(reaped)
	r := StartOutputRelease(OutputReleaseParams{
		Pipes:      newTestOwnedPipes(t),
		Reaped:     reaped,
		ReaderDone: make(chan struct{}),
		Grace:      0,
		Logger:     slog.New(slog.DiscardHandler),
	})

	const tooEarly = 500 * time.Millisecond
	select {
	case <-r.Abandoned():
		t.Fatalf("the release abandoned within %v of a zero Grace, want it to have resolved to DefaultDrainGrace (%v) rather than firing immediately", tooEarly, DefaultDrainGrace)
	case <-time.After(tooEarly):
	}

	select {
	case <-r.Abandoned():
	case <-time.After(DefaultDrainGrace):
		t.Fatal("the release never abandoned, want it bounded by DefaultDrainGrace")
	}
}
