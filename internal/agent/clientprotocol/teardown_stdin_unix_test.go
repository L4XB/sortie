//go:build unix

package clientprotocol

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// blockingCloser is an io.Closer whose Close blocks until the test
// releases it, standing in for a stdin handle whose OS-level close
// never returns.
type blockingCloser struct {
	release chan struct{}
}

func (c blockingCloser) Close() error {
	<-c.release
	return nil
}

// TestStopSession_StdinCloseNeverReturns drives stopSession against a
// real subprocess, with state.stdinCloser replaced by a handle whose
// Close blocks forever. stopSession must still return within its
// configured grace, proving close_stdin never waits on that close.
func TestStopSession_StdinCloseNeverReturns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	state := newGracefulTeardownSession(t, teardownIgnoresGracefulScript(readyPath), readyPath, nil)
	state.agentConfig.StopGraceMS = 200

	release := make(chan struct{})
	state.stdinCloser = blockingCloser{release: release}
	t.Cleanup(func() { close(release) })

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- stopSession(context.Background(), fakeSession(state)) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopSession() error = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("stopSession() took %v, want well under 3s (a stdin Close that never returns must not block teardown)", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopSession() did not return within 5s against a stdin Close that never returns")
	}
}
