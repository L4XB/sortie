package procutil

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// blockingCloser is an io.Closer whose Close blocks until the test
// releases it, and records whether Close ever ran.
type blockingCloser struct {
	release chan struct{}
	closed  chan struct{}
}

func newBlockingCloser() *blockingCloser {
	return &blockingCloser{release: make(chan struct{}), closed: make(chan struct{})}
}

func (c *blockingCloser) Close() error {
	<-c.release
	close(c.closed)
	return nil
}

// TestCloseWithoutWaiting_ReturnsAtOnceForABlockingCloser checks that
// CloseWithoutWaiting returns before a closer whose Close blocks
// forever ever unblocks.
func TestCloseWithoutWaiting_ReturnsAtOnceForABlockingCloser(t *testing.T) {
	t.Parallel()

	closer := newBlockingCloser()

	returned := make(chan struct{})
	go func() {
		CloseWithoutWaiting(closer)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseWithoutWaiting() did not return within 2s against a closer whose Close blocks forever")
	}

	select {
	case <-closer.closed:
		t.Fatal("closer.Close() completed before the test released it, want CloseWithoutWaiting to have returned first regardless")
	default:
	}

	close(closer.release)
	select {
	case <-closer.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closer.Close() did not complete after being released")
	}
}

// countingCloser records how many times Close was called and returns
// the configured error every time. calls is an atomic counter because
// CloseWithoutWaiting runs Close on its own goroutine, concurrently
// with the test goroutine polling for it.
type countingCloser struct {
	calls atomic.Int64
	err   error
}

func (c *countingCloser) Close() error {
	c.calls.Add(1)
	return c.err
}

// TestCloseWithoutWaiting_StillClosesAnOrdinaryCloser checks that an
// ordinary, non-blocking closer is still closed exactly once.
func TestCloseWithoutWaiting_StillClosesAnOrdinaryCloser(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("close failed")
	closer := &countingCloser{err: wantErr}

	done := make(chan struct{})
	go func() {
		CloseWithoutWaiting(closer)
		close(done)
	}()

	// CloseWithoutWaiting runs Close on its own goroutine, so its own
	// return gives no synchronization with that goroutine's Close call;
	// poll for the call to land instead of asserting on a single sample.
	deadline := time.Now().Add(2 * time.Second)
	for closer.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("closer.Close() was never called within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := closer.calls.Load(); got != 1 {
		t.Errorf("closer.Close() was called %d times, want 1", got)
	}
}

// TestCloseWithoutWaiting_NilDoesNothing checks that a nil closer is
// tolerated and causes no panic.
func TestCloseWithoutWaiting_NilDoesNothing(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	go func() {
		CloseWithoutWaiting(nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseWithoutWaiting(nil) did not return within 2s")
	}
}
