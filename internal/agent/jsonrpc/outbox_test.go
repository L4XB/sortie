package jsonrpc

import (
	"errors"
	"io"
	"testing"
	"time"
)

// failingWriter fails every write with err.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestInbox_PutReportsWhetherAccepted(t *testing.T) {
	t.Parallel()

	inbox := NewInbox[int]()
	if !inbox.Put(1) {
		t.Error("Put() on an open inbox = false, want true")
	}
	inbox.Close()
	if inbox.Put(2) {
		t.Error("Put() after Close = true, want false")
	}
}

// TestConn_WriteFailureClosesOutbox checks that once a write fails and
// the writer goroutine exits, the outbox refuses further lines, so a
// send racing that failure reports it instead of queueing a line
// nothing will ever write.
func TestConn_WriteFailureClosesOutbox(t *testing.T) {
	t.Parallel()

	peerR, peerW := io.Pipe()
	t.Cleanup(func() {
		_ = peerW.Close()
		_ = peerR.Close()
	})
	conn := NewConn(failingWriter{err: errors.New("broken pipe")}, peerR, Deliver(NewInbox[Message](), func(m Message) Message { return m }))
	t.Cleanup(conn.Close)

	if err := conn.Notify("first", nil); err != nil {
		t.Fatalf("Notify() before any write failed = %v, want nil", err)
	}
	select {
	case <-conn.WriteFailed():
	case <-time.After(2 * time.Second):
		t.Fatal("WriteFailed() did not close within 2s after the writer failed")
	}

	// failWrite closes the outbox just after writeFailedCh, so poll for
	// the refusal rather than asserting on a single sample.
	deadline := time.Now().Add(2 * time.Second)
	for conn.outbox.Put(outboxItem{line: []byte("late\n")}) {
		if time.Now().After(deadline) {
			t.Fatal("outbox still accepted lines 2s after the write failure, want it refused once the writer goroutine exited")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
