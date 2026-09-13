//go:build unix

package codex

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestRunTurn_EndsOnCtxDeadlineAgainstAStalledWriter drives RunTurn
// with its connection wired to an io.Pipe whose read end nobody ever
// reads, so the turn/start line it writes can never complete at the OS
// level, and a second io.Pipe whose write end nobody ever writes to,
// so the reader stays open rather than reporting a stream end. Only
// the turn's own ctx deadline can end this turn; the adapter's write
// itself never returns.
func TestRunTurn_EndsOnCtxDeadlineAgainstAStalledWriter(t *testing.T) {
	t.Parallel()

	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	t.Cleanup(func() {
		_ = outR.Close()
		_ = outW.Close()
		_ = inR.Close()
		_ = inW.Close()
	})

	inbox := jsonrpc.NewInbox[jsonrpc.Message]()
	state := &sessionState{
		threadID:   "thread-001",
		target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		waitCh:     make(chan struct{}),
		inbox:      inbox,
		readerDone: make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
	}
	state.conn = jsonrpc.NewConn(outW, inR, jsonrpc.Deliver(inbox, identity))
	go watchTermination(state)

	adapter, _ := NewCodexAdapter(map[string]any{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	done := make(chan struct {
		result domain.TurnResult
		err    error
	}, 1)
	go func() {
		result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
			Prompt:  "go",
			OnEvent: func(domain.AgentEvent) {},
		})
		done <- struct {
			result domain.TurnResult
			err    error
		}{result, err}
	}()

	select {
	case got := <-done:
		elapsed := time.Since(start)
		requireAgentError(t, got.err, domain.ErrPortExit)
		if got.result.UsageMeasured {
			t.Error("RunTurn().UsageMeasured = true, want false")
		}
		if elapsed > 5*time.Second {
			t.Errorf("RunTurn() took %v, want close to the 200ms ctx deadline (the stalled write must never be what it waits on)", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunTurn() did not return within 10s against a stalled writer, want it ended by its own ctx deadline")
	}
}

// blockingStdin is an io.WriteCloser whose Close blocks until the test
// releases it, standing in for a stdin handle whose OS-level close
// never returns.
type blockingStdin struct {
	release chan struct{}
}

func (blockingStdin) Write(p []byte) (int, error) { return len(p), nil }

func (b blockingStdin) Close() error {
	<-b.release
	return nil
}

// TestStopSession_StdinCloseNeverReturns drives StopSession against a
// real subprocess that ignores the graceful signal, with state.stdin
// replaced by a handle whose Close blocks forever. StopSession must
// still send the graceful signal, reach the escalation kill, and
// return within its configured grace, proving closing stdin cannot
// hold up the rest of teardown.
func TestStopSession_StdinCloseNeverReturns(t *testing.T) {
	t.Parallel()

	state := startFakeCodexProcess(t, `trap '' TERM
while :; do :; done`, 200)

	release := make(chan struct{})
	state.stdin = blockingStdin{release: release}
	t.Cleanup(func() { close(release) })

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state}) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("StopSession() error = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("StopSession() took %v, want well under 3s (a stdin Close that never returns must not block the signal, wait, or kill that follow it)", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopSession() did not return within 5s against a stdin Close that never returns")
	}
}
