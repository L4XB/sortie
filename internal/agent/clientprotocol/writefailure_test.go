package clientprotocol

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

// alwaysFailWriter fails every Write with err, standing in for a
// connection whose writer goroutine can never deliver anything.
type alwaysFailWriter struct{ err error }

func (w alwaysFailWriter) Write(p []byte) (int, error) { return 0, w.err }

// newFailWriteSession builds a *sessionState like newTestSession, but
// with its connection's writer replaced by one that fails every write,
// so a turn's own prompt send fails asynchronously once
// handleStartTurn's SendRequest has already enqueued and returned.
func newFailWriteSession(t *testing.T, agentConfig domain.AgentConfig, writeErr error) (*sessionState, *io.PipeWriter) {
	t.Helper()

	inPr, inPw := io.Pipe()
	state := &sessionState{
		agentConfig: agentConfig,
		caps:        newCapabilityRecord(false),
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      discardLogger(),
		origins:     &sessionOrigins{},
	}
	state.inbox = jsonrpc.NewInbox[pumpItem]()
	state.conn = jsonrpc.NewConn(alwaysFailWriter{err: writeErr}, inPr, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(8<<20))

	go runPump(state)
	t.Cleanup(func() {
		_ = inPw.Close()
		state.stopOnce.Do(func() { close(state.stopCh) })
		<-state.pumpDone
		_ = inPr.Close()
	})

	return state, inPw
}

// TestPump_WriteFailureDuringTurn_StreamEndWinsWithinBound checks that
// when the runtime's stream ends within agent.read_timeout_ms of a
// write failure, the stream-end path finalizes the turn (its
// process-exit row), not the send-failure message.
func TestPump_WriteFailureDuringTurn_StreamEndWinsWithinBound(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("boom")
	state, inPw := newFailWriteSession(t, domain.AgentConfig{ReadTimeoutMS: 300}, writeErr)
	markSessionKnown(state)

	turnCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})

	// Wait for the prompt send's write to actually fail before ending
	// the stream, so the turn is genuinely active (its prompt already
	// sent) when the stream ends, rather than racing the turn's own
	// acceptance and being rejected as never having started.
	select {
	case <-state.conn.WriteFailed():
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for the prompt send's write to fail")
	}

	_ = inPw.Close()

	outcome := awaitOutcome(t, turnCh)
	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) {
		t.Fatalf("runTurn() error = %v (%T), want *domain.AgentError", outcome.err, outcome.err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if agentErr.Message != streamEndedMessage {
		t.Errorf("AgentError.Message = %q, want %q (the stream-end path, not the send-failure message)", agentErr.Message, streamEndedMessage)
	}
}

// TestPump_WriteFailureDuringTurn_SendFailureWinsAfterBound checks that
// when the reader stays alive past agent.read_timeout_ms with the
// write failure unresolved, the turn ends with the send-failure
// outcome, and only once that bound has actually elapsed.
func TestPump_WriteFailureDuringTurn_SendFailureWinsAfterBound(t *testing.T) {
	t.Parallel()

	const bound = 300 * time.Millisecond
	writeErr := errors.New("boom")
	state, _ := newFailWriteSession(t, domain.AgentConfig{ReadTimeoutMS: int(bound.Milliseconds())}, writeErr)
	markSessionKnown(state)

	start := time.Now()
	turnCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})

	outcome := awaitOutcome(t, turnCh)
	elapsed := time.Since(start)

	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) {
		t.Fatalf("runTurn() error = %v (%T), want *domain.AgentError", outcome.err, outcome.err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if agentErr.Message != promptSendFailedMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, promptSendFailedMessage)
	}
	if elapsed < bound/2 {
		t.Errorf("runTurn() ended after %v, want it to wait close to the %v bound rather than finalizing at once", elapsed, bound)
	}
	if elapsed > awaitTimeout {
		t.Errorf("runTurn() took %v, want under %v", elapsed, awaitTimeout)
	}
}

// TestPumpState_WriteFailureWithNoActiveTurn checks that a write
// failure observed with no turn active neither arms a deadline nor
// does anything once handled: armWriteFailedDeadline reports no
// deadline, and handleWriteFailed is a safe no-op.
func TestPumpState_WriteFailureWithNoActiveTurn(t *testing.T) {
	t.Parallel()

	p := &pumpState{}

	if ch := p.armWriteFailedDeadline(); ch != nil {
		t.Errorf("armWriteFailedDeadline() = %v, want nil when no turn is active", ch)
	}

	p.handleWriteFailed()

	if p.activeTurn != nil {
		t.Error("activeTurn became non-nil from handleWriteFailed with none active, want it to stay nil")
	}
}
