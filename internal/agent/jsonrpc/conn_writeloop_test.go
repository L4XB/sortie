package jsonrpc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
)

// neverReadPipe returns an io.Writer whose reader end is never read,
// so any write to it blocks forever until the pipe is closed. The
// returned closer must be closed by the caller once the test no
// longer needs the write to stay parked.
func neverReadPipe(t *testing.T) (w io.Writer, closePipe func()) {
	t.Helper()
	pr, pw := io.Pipe()
	return pw, func() {
		_ = pr.Close()
		_ = pw.Close()
	}
}

// neverWrittenReader returns an io.Reader whose Read blocks forever,
// because nothing ever writes to the pipe behind it: unlike
// strings.NewReader(""), which reports a clean EOF the instant it is
// read and would close a Conn's Done() channel almost at once, this
// never gives the reader loop anything to observe, so only the
// condition a test is actually exercising can end a Call. The
// returned closer must be closed by the caller once the test is done.
func neverWrittenReader(t *testing.T) (r io.Reader, closePipe func()) {
	t.Helper()
	pr, pw := io.Pipe()
	return pr, func() {
		_ = pr.Close()
		_ = pw.Close()
	}
}

// TestConn_CallReturnsOnCtxCancelWhilePeerNeverReads checks that Call
// returns once its context is cancelled even while the peer never
// reads the request it enqueued, proving the write itself does not
// block the caller.
func TestConn_CallReturnsOnCtxCancelWhilePeerNeverReads(t *testing.T) {
	t.Parallel()

	w, closeWritePipe := neverReadPipe(t)
	t.Cleanup(closeWritePipe)
	r, closeReadPipe := neverWrittenReader(t)
	t.Cleanup(closeReadPipe)

	conn := jsonrpc.NewConn(w, r, discardSink())
	t.Cleanup(conn.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan callOutcome, 1)
	go func() {
		resp, err := conn.Call(ctx, "call/method", nil)
		done <- callOutcome{resp: resp, err: err}
	}()

	cancel()

	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Errorf("Call() error = %v, want error wrapping context.Canceled", outcome.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call() did not return within 2s after ctx cancel while the peer never read the request")
	}
}

// TestConn_EnqueueingMethodsReturnAtOnceWhilePeerNeverReads checks that
// Notify, Respond, RespondError, and SendRequest each return without
// waiting for the peer to read the line, unlike Call, which they never
// even ask to.
func TestConn_EnqueueingMethodsReturnAtOnceWhilePeerNeverReads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		do   func(conn *jsonrpc.Conn) error
	}{
		{"Notify", func(conn *jsonrpc.Conn) error { return conn.Notify("parked/notify", nil) }},
		{"Respond", func(conn *jsonrpc.Conn) error { return conn.Respond(jsonrpc.NumberID(1), map[string]any{"ok": true}) }},
		{"RespondError", func(conn *jsonrpc.Conn) error { return conn.RespondError(jsonrpc.NumberID(1), -32000, "denied") }},
		{"SendRequest", func(conn *jsonrpc.Conn) error { _, err := conn.SendRequest("parked/request", nil); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, closePipe := neverReadPipe(t)
			t.Cleanup(closePipe)
			conn := jsonrpc.NewConn(w, strings.NewReader(""), discardSink())
			t.Cleanup(conn.Close)

			done := make(chan error, 1)
			go func() { done <- tt.do(conn) }()

			select {
			case err := <-done:
				if err != nil {
					t.Errorf("%s() error = %v, want nil", tt.name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s() did not return within 2s while the peer never read the pipe", tt.name)
			}
		})
	}
}

// TestConn_EnqueuedLinesPreserveOrder checks that a sequence of
// enqueued notifications reaches the underlying writer in the exact
// order they were enqueued, one complete line at a time.
func TestConn_EnqueuedLinesPreserveOrder(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	conn := jsonrpc.NewConn(&buf, strings.NewReader(""), discardSink())
	t.Cleanup(conn.Close)

	const n = 50
	for i := range n {
		if err := conn.Notify("order/test", map[string]any{"i": i}); err != nil {
			t.Fatalf("Notify(%d) error = %v", i, err)
		}
	}
	if err := conn.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("wrote %d lines, want %d", len(lines), n)
	}
	for i, line := range lines {
		var wire struct {
			Params struct {
				I int `json:"i"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &wire); err != nil {
			t.Fatalf("unmarshal line %d (%s): %v", i, line, err)
		}
		if wire.Params.I != i {
			t.Errorf("line %d carries params.i = %d, want %d (lines arrived out of enqueue order)", i, wire.Params.I, i)
		}
	}
}

// countingFailWriter fails every Write with err and counts how many
// times Write was called, so a test can assert that a write failure
// ends further attempts rather than merely reporting the same error
// repeatedly.
type countingFailWriter struct {
	calls atomic.Int64
	err   error
}

func (w *countingFailWriter) Write(p []byte) (int, error) {
	w.calls.Add(1)
	return 0, w.err
}

// TestConn_WriteFailureIsTerminal checks that a write failure fails a
// pending Call with an error wrapping the writer's own error, closes
// WriteFailed, and makes every later send fail at the enqueue check
// without touching the writer again.
func TestConn_WriteFailureIsTerminal(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("boom")
	w := &countingFailWriter{err: writeErr}
	r, closeReadPipe := neverWrittenReader(t)
	t.Cleanup(closeReadPipe)
	conn := jsonrpc.NewConn(w, r, discardSink())
	t.Cleanup(conn.Close)

	done := make(chan callOutcome, 1)
	go func() {
		resp, err := conn.Call(context.Background(), "call/method", nil)
		done <- callOutcome{resp: resp, err: err}
	}()

	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, writeErr) {
			t.Errorf("Call() error = %v, want error wrapping %v", outcome.err, writeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call() did not fail after the write behind it failed")
	}

	select {
	case <-conn.WriteFailed():
	case <-time.After(2 * time.Second):
		t.Fatal("WriteFailed() did not close after a write failure")
	}

	if got := w.calls.Load(); got != 1 {
		t.Fatalf("writer was called %d times before later sends were attempted, want 1", got)
	}

	if err := conn.Notify("later/notify", nil); !errors.Is(err, writeErr) {
		t.Errorf("Notify() after write failure = %v, want error wrapping %v", err, writeErr)
	}
	if err := conn.Respond(jsonrpc.NumberID(9), map[string]any{}); !errors.Is(err, writeErr) {
		t.Errorf("Respond() after write failure = %v, want error wrapping %v", err, writeErr)
	}
	if err := conn.RespondError(jsonrpc.NumberID(9), -32000, "x"); !errors.Is(err, writeErr) {
		t.Errorf("RespondError() after write failure = %v, want error wrapping %v", err, writeErr)
	}
	if _, err := conn.SendRequest("later/request", nil); !errors.Is(err, writeErr) {
		t.Errorf("SendRequest() after write failure = %v, want error wrapping %v", err, writeErr)
	}
	if _, err := conn.Call(context.Background(), "later/call", nil); !errors.Is(err, writeErr) {
		t.Errorf("Call() after write failure = %v, want error wrapping %v", err, writeErr)
	}

	if got := w.calls.Load(); got != 1 {
		t.Errorf("writer was called %d times after the terminal failure, want 1 (later sends must fail without writing)", got)
	}
}

// releasableWriter blocks its first Write until release is closed, and
// closes entered the instant that Write is called, so a test can
// observe the write in progress before choosing to let it complete.
type releasableWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newReleasableWriter() *releasableWriter {
	return &releasableWriter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *releasableWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

// TestConn_Flush covers Flush's three documented outcomes: it waits
// for an in-progress write and then returns nil, it returns the ctx
// error when the peer never reads, and it reports ErrClosed once the
// connection has been closed.
func TestConn_Flush(t *testing.T) {
	t.Parallel()

	t.Run("returns nil only once the in-progress write finishes", func(t *testing.T) {
		t.Parallel()

		w := newReleasableWriter()
		conn := jsonrpc.NewConn(w, strings.NewReader(""), discardSink())
		t.Cleanup(conn.Close)

		if err := conn.Notify("slow/notify", nil); err != nil {
			t.Fatalf("Notify() error = %v", err)
		}

		select {
		case <-w.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the write to start")
		}

		flushDone := make(chan error, 1)
		go func() { flushDone <- conn.Flush(context.Background()) }()

		select {
		case err := <-flushDone:
			t.Fatalf("Flush() returned (err=%v) before the in-progress write finished", err)
		case <-time.After(200 * time.Millisecond):
		}

		close(w.release)

		select {
		case err := <-flushDone:
			if err != nil {
				t.Errorf("Flush() error = %v, want nil", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Flush() did not return after the write finished")
		}
	})

	t.Run("returns the ctx error when the peer never reads", func(t *testing.T) {
		t.Parallel()

		w, closePipe := neverReadPipe(t)
		t.Cleanup(closePipe)
		conn := jsonrpc.NewConn(w, strings.NewReader(""), discardSink())
		t.Cleanup(conn.Close)

		if err := conn.Notify("parked/notify", nil); err != nil {
			t.Fatalf("Notify() error = %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		flushDone := make(chan error, 1)
		go func() { flushDone <- conn.Flush(ctx) }()

		cancel()

		select {
		case err := <-flushDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Flush() error = %v, want error wrapping context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Flush() did not return after ctx cancel while the peer never read the pipe")
		}
	})

	t.Run("returns an error wrapping ErrClosed after Close", func(t *testing.T) {
		t.Parallel()

		conn := jsonrpc.NewConn(io.Discard, strings.NewReader(""), discardSink())
		conn.Close()

		if err := conn.Flush(context.Background()); !errors.Is(err, jsonrpc.ErrClosed) {
			t.Errorf("Flush() after Close() = %v, want error wrapping jsonrpc.ErrClosed", err)
		}
	})
}
