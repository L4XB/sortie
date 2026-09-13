//go:build unix

package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*CodexAdapter)(nil)

// requireAgentError asserts err is a *domain.AgentError with the given Kind.
func requireAgentError(t *testing.T, err error, wantKind domain.AgentErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", wantKind)
	}
	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if ae.Kind != wantKind {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, wantKind)
	}
}

func TestNewCodexAdapter(t *testing.T) {
	t.Parallel()

	t.Run("nil config returns adapter", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCodexAdapter(nil)
		if err != nil {
			t.Fatalf("NewCodexAdapter(nil) error = %v", err)
		}
		if adapter == nil {
			t.Fatal("adapter is nil")
		}
	})

	t.Run("empty config returns adapter", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCodexAdapter(map[string]any{})
		if err != nil {
			t.Fatalf("NewCodexAdapter(empty) error = %v", err)
		}
		if adapter == nil {
			t.Fatal("adapter is nil")
		}
	})

	t.Run("all passthrough fields stored", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCodexAdapter(map[string]any{
			"model":           "o4-mini",
			"effort":          "high",
			"approval_policy": "never",
			"thread_sandbox":  "workspaceWrite",
			"personality":     "helpful",
		})
		if err != nil {
			t.Fatalf("NewCodexAdapter() error = %v", err)
		}
		a := adapter.(*CodexAdapter)
		if a.passthrough.Model != "o4-mini" {
			t.Errorf("passthrough.Model = %q, want %q", a.passthrough.Model, "o4-mini")
		}
		if a.passthrough.Effort != "high" {
			t.Errorf("passthrough.Effort = %q, want %q", a.passthrough.Effort, "high")
		}
		if a.passthrough.ApprovalPolicy != "never" {
			t.Errorf("passthrough.ApprovalPolicy = %q, want %q", a.passthrough.ApprovalPolicy, "never")
		}
		if a.passthrough.ThreadSandbox != "workspaceWrite" {
			t.Errorf("passthrough.ThreadSandbox = %q, want %q", a.passthrough.ThreadSandbox, "workspaceWrite")
		}
		if a.passthrough.Personality != "helpful" {
			t.Errorf("passthrough.Personality = %q, want %q", a.passthrough.Personality, "helpful")
		}
	})

	t.Run("tool_registry config key is not read", func(t *testing.T) {
		t.Parallel()
		reg := domain.NewToolRegistry()
		withKey, err := NewCodexAdapter(map[string]any{
			"tool_registry": reg,
		})
		if err != nil {
			t.Fatalf("NewCodexAdapter() error = %v", err)
		}
		without, err := NewCodexAdapter(map[string]any{})
		if err != nil {
			t.Fatalf("NewCodexAdapter() error = %v", err)
		}
		a := withKey.(*CodexAdapter)
		b := without.(*CodexAdapter)
		if !reflect.DeepEqual(a.passthrough, b.passthrough) {
			t.Errorf("adapter constructed with tool_registry present = %+v, want identical to %+v", a.passthrough, b.passthrough)
		}
	})
}

func TestRegistration(t *testing.T) {
	t.Parallel()

	factory, err := registry.Agents.Get("codex")
	if err != nil {
		t.Fatalf(`registry.Agents.Get("codex") error = %v`, err)
	}
	adapter, err := factory(map[string]any{})
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if _, ok := adapter.(*CodexAdapter); !ok {
		t.Errorf("factory() type = %T, want *CodexAdapter", adapter)
	}
}

func TestStartSession_EmptyWorkspace(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_NonexistentPath(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: "/nonexistent/path/that/does/not/exist/codex-test",
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_WorkspaceIsFile(t *testing.T) {
	t.Parallel()

	tmpFile := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpFile,
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_BinaryNotFound(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-codex-binary-99999"},
	})
	requireAgentError(t, err, domain.ErrAgentNotFound)
}

// TestRunTurn_UsageMeasured_AbsentWhenNoTokenUsageNotification drives a
// turn whose stream carries no thread/tokenUsage/updated notification and
// asserts the run is reported unmeasured.
func TestRunTurn_UsageMeasured_AbsentWhenNoTokenUsageNotification(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_misc_notifications.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	agenttest.AssertMeasurementAbsent(t, events, result)
}

// TestRunTurn_UsageMeasured_TrueOnTokenUsageNotification drives a turn
// whose stream carries a thread/tokenUsage/updated notification and
// asserts the run is reported measured.
func TestRunTurn_UsageMeasured_TrueOnTokenUsageNotification(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if !result.UsageMeasured {
		t.Error("RunTurn().UsageMeasured = false, want true when a thread/tokenUsage/updated notification carried a token-usage object")
	}
}

// TestRunTurn_CompletedTurnReturnsUntypedNilError pins that a completed
// turn's returned error interface is genuinely nil, not a typed-nil
// *domain.AgentError promoted to a non-nil error interface.
func TestRunTurn_CompletedTurnReturnsUntypedNilError(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	_, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Errorf("RunTurn() error = %v, want nil (not a typed-nil *domain.AgentError)", err)
	}
}

// atomicErrContext behaves like context.Background() to every consumer
// that selects on Done() (the channel is nil and never closes), but its
// Err() method reports context.Canceled once cancelled is set, so a
// test can drive a code path that reads ctx.Err() synchronously without
// racing RunTurn's own <-ctx.Done() select case.
type atomicErrContext struct {
	context.Context
	cancelled *atomic.Bool
}

func (c atomicErrContext) Err() error {
	if c.cancelled.Load() {
		return context.Canceled
	}
	return nil
}

// newInterruptedStatusState builds a sessionState wired to a real
// jsonrpc.Conn over an io.Pipe, whose peer answers the turn/start call
// only once it observes codex write it (required so the response
// cannot be misrouted as unmatched, ahead of Call's own pending-map
// registration). stdin, when non-nil, receives every line codex
// writes, including the turn/start request itself.
func newInterruptedStatusState(t *testing.T, stdin io.Writer) *sessionState {
	t.Helper()

	sig := newSignalingWriter(stdin)
	inPr, inPw := io.Pipe()
	t.Cleanup(func() {
		_ = inPr.Close()
		_ = inPw.Close()
	})

	state := &sessionState{
		threadID:   "thread-001",
		target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		waitCh:     make(chan struct{}),
		inbox:      jsonrpc.NewInbox[jsonrpc.Message](),
		readerDone: make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
	}
	state.conn = jsonrpc.NewConn(sig, inPr, jsonrpc.Deliver(state.inbox, identity))
	go watchTermination(state)

	go func() {
		<-sig.done
		_, _ = fmt.Fprintln(inPw, `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}`)
	}()

	return state
}

// waitForSessionStarted blocks until state's turn emits its
// domain.EventSessionStarted event, which RunTurn's loop can only
// reach after its initial turn/start Call has returned: this is the
// test's proof that RunTurn has moved past that call and into its main
// loop, so a real context cancellation that follows lands on the
// loop's own arm rather than aborting the call itself.
func waitForSessionStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTurn did not emit domain.EventSessionStarted within 2s, want it past the initial turn/start call")
	}
}

// onEventSignalingSessionStarted wraps onEvent so it closes started
// the first time it observes domain.EventSessionStarted, in addition
// to forwarding every event to onEvent unchanged.
func onEventSignalingSessionStarted(onEvent func(domain.AgentEvent), started chan<- struct{}) func(domain.AgentEvent) {
	var once sync.Once
	return func(e domain.AgentEvent) {
		if e.Type == domain.EventSessionStarted {
			once.Do(func() { close(started) })
		}
		onEvent(e)
	}
}

// TestRunTurn_InterruptedStatus pins the context-gated mapping for a
// turn/completed notification reporting status interrupted: cancelled
// only when the turn context is already done, failed (preserving the
// retryable classification) when the context is still live.
func TestRunTurn_InterruptedStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		contextIsLive bool
		wantEvidence  agentcore.TurnEvidence
	}{
		{
			name:          "live context maps to failure, preserving the retryable classification",
			contextIsLive: true,
			wantEvidence: agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrTurnFailed,
				TerminalMessage:   "turn interrupted",
			},
		},
		{
			name:          "already-cancelled context maps to cancellation",
			contextIsLive: false,
			wantEvidence: agentcore.TurnEvidence{
				Terminal:        agentcore.TerminalCancelled,
				TerminalMessage: "turn cancelled after the runtime reported status interrupted",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newInterruptedStatusState(t, nil)

			var cancelled atomic.Bool
			ctx := atomicErrContext{Context: context.Background(), cancelled: &cancelled}

			type outcome struct {
				result domain.TurnResult
				err    error
			}
			outcomeCh := make(chan outcome, 1)

			adapter, _ := NewCodexAdapter(map[string]any{})
			go func() {
				result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
					Prompt:  "go",
					OnEvent: func(domain.AgentEvent) {},
				})
				outcomeCh <- outcome{result, err}
			}()

			if !tt.contextIsLive {
				cancelled.Store(true)
			}

			// Put and the RunTurn loop's Take are both serialized through
			// the inbox's own mutex, so everything the test did above,
			// including setting the cancellation flag, is visible once
			// RunTurn takes this item and reads ctx.Err() while handling
			// it.
			state.inbox.Put(jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"turn-001","status":"interrupted"}}`)})

			got := <-outcomeCh
			result, err := got.result, got.err

			dispositiontest.AssertDispositionContract(t, tt.wantEvidence, result, err)
		})
	}
}

// TestRunTurn_CompletedNotificationUnderCancelledContext pins the
// cancelled-context mapping table: whatever status word (or absence of
// one) the runtime reports in a turn/completed notification once the
// orchestrator has already cancelled the turn, the disposition is a
// cancellation carrying a message that names both facts.
func TestRunTurn_CompletedNotificationUnderCancelledContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		params      string
		wantMessage string
	}{
		{
			name:        "completed status with no error object",
			params:      `{"turn":{"id":"turn-001","status":"completed"}}`,
			wantMessage: "turn cancelled after the runtime reported status completed",
		},
		{
			name:        "interrupted status with no error object",
			params:      `{"turn":{"id":"turn-001","status":"interrupted"}}`,
			wantMessage: "turn cancelled after the runtime reported status interrupted",
		},
		{
			name:        "failed status carrying a turn.error object",
			params:      `{"turn":{"id":"turn-001","status":"failed","error":{"message":"context window exceeded","codexErrorInfo":"ContextWindowExceeded"}}}`,
			wantMessage: "turn cancelled after the runtime reported status failed: context window exceeded",
		},
		{
			name:        "unrecognized status",
			params:      `{"turn":{"id":"turn-001","status":"queued_for_review"}}`,
			wantMessage: "turn cancelled after the runtime reported status queued_for_review",
		},
		{
			name:        "payload with no status member",
			params:      `{"turn":{"id":"turn-001"}}`,
			wantMessage: "turn cancelled after the runtime reported no status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newInterruptedStatusState(t, nil)

			var cancelled atomic.Bool
			ctx := atomicErrContext{Context: context.Background(), cancelled: &cancelled}

			type outcome struct {
				result domain.TurnResult
				err    error
			}
			outcomeCh := make(chan outcome, 1)

			adapter, _ := NewCodexAdapter(map[string]any{})
			go func() {
				result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
					Prompt:  "go",
					OnEvent: func(domain.AgentEvent) {},
				})
				outcomeCh <- outcome{result, err}
			}()

			cancelled.Store(true)

			// Put and the RunTurn loop's Take are both serialized through
			// the inbox's own mutex, so everything the test did above,
			// including setting the cancellation flag, is visible once
			// RunTurn takes this item and reads ctx.Err() while handling
			// it.
			state.inbox.Put(jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/completed", Params: json.RawMessage(tt.params)})

			got := <-outcomeCh
			result, err := got.result, got.err

			dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
				Terminal:        agentcore.TerminalCancelled,
				TerminalMessage: tt.wantMessage,
			}, result, err)
		})
	}
}

// TestRunTurn_FailedOrUnrecognizedStatus pins that a failed status with
// no turn.error object, and any status other than completed,
// interrupted, and failed, both produce turn_failed with a non-nil
// error and the message "turn <status>", unchanged from before the
// shared decision.
func TestRunTurn_FailedOrUnrecognizedStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		wantMessage string
	}{
		{
			name:        "failed status with no turn.error object",
			fixture:     `{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"failed"}}}` + "\n",
			wantMessage: "turn failed",
		},
		{
			name:        "unrecognized status",
			fixture:     `{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"queued_for_review"}}}` + "\n",
			wantMessage: "turn queued_for_review",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}` + "\n" + tt.fixture
			state := makeTestState(t, []byte(fixture))
			adapter, _ := NewCodexAdapter(map[string]any{})

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})

			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) {
				t.Fatalf("error type = %T, want *domain.AgentError", err)
			}
			if agentErr.Kind != domain.ErrTurnFailed {
				t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrTurnFailed)
			}
			if agentErr.Message != tt.wantMessage {
				t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, tt.wantMessage)
			}

			dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrTurnFailed,
				TerminalMessage:   tt.wantMessage,
			}, result, err)
		})
	}
}

// TestRunTurn_EmptyTurnStatus pins both ways a turn/completed
// notification can arrive without a status word: params that fail to
// unmarshal into an object, and a turn object that omits the status
// member. Neither builds a terminal message from the empty status, so
// agentcore.DecideTurn's own fallback message applies.
func TestRunTurn_EmptyTurnStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params string
	}{
		{"params do not unmarshal into an object", `[1,2,3]`},
		{"turn object omits the status member", `{"turn":{"id":"turn-001"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}` + "\n" +
				`{"method":"turn/completed","params":` + tt.params + `}` + "\n"
			state := makeTestState(t, []byte(fixture))
			adapter, _ := NewCodexAdapter(map[string]any{})

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})

			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) {
				t.Fatalf("error type = %T, want *domain.AgentError", err)
			}
			if agentErr.Kind != domain.ErrTurnFailed {
				t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrTurnFailed)
			}
			if agentErr.Message != "turn failed" {
				t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, "turn failed")
			}

			dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrTurnFailed,
			}, result, err)
		})
	}
}

// TestRunTurn_StdoutParseFailure pins that a stdout line that fails to
// parse produces the same text on both the emitted event and the
// returned error, prefixed "stdout read error: ", and preserves the
// underlying parse error on the unwrap chain.
func TestRunTurn_StdoutParseFailure(t *testing.T) {
	t.Parallel()

	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"not valid json\n"
	state := makeTestState(t, []byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	})

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if !strings.HasPrefix(agentErr.Message, "stdout read error: ") {
		t.Errorf("AgentError.Message = %q, want prefix %q", agentErr.Message, "stdout read error: ")
	}

	turnFailedEvents := filterEventsOfType(events, domain.EventTurnFailed)
	if len(turnFailedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(turnFailedEvents))
	}
	if turnFailedEvents[0].Message != agentErr.Message {
		t.Errorf("turn_failed Message = %q, want the same text as AgentError.Message %q", turnFailedEvents[0].Message, agentErr.Message)
	}
}

// entrySignalingWriter wraps an io.Writer and closes done the instant
// Write is entered, before delegating to the wrapped writer. Unlike
// signalingWriter, which signals once Write returns, this lets a test
// observe that a write has been parked inside a call that never
// returns on its own.
type entrySignalingWriter struct {
	w    io.Writer
	once sync.Once
	done chan struct{}
}

func newEntrySignalingWriter(w io.Writer) *entrySignalingWriter {
	return &entrySignalingWriter{w: w, done: make(chan struct{})}
}

func (s *entrySignalingWriter) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.done) })
	return s.w.Write(p)
}

// TestStopSession_ReturnsWhileWriteParked checks that StopSession
// returns while another goroutine is parked writing on the session's
// connection, proving that closeConn's call to conn.Close does not
// wait for that write. The fixture leaves no process handle and no
// wait channel, and readerDone already closed, so no other bounded
// wait in StopSession can substitute for that proof.
func TestStopSession_ReturnsWhileWriteParked(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	sig := newEntrySignalingWriter(pw)

	readerDone := make(chan struct{})
	close(readerDone)

	state := &sessionState{
		inbox:      jsonrpc.NewInbox[jsonrpc.Message](),
		readerDone: readerDone,
	}
	state.conn = jsonrpc.NewConn(sig, strings.NewReader(""), jsonrpc.Deliver(state.inbox, identity))

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- state.conn.Notify("parked/notify", nil)
	}()

	// Registered before the assertions below, so a t.Fatal on the
	// regression path still releases the parked write.
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
		select {
		case <-writeErr:
		case <-time.After(2 * time.Second):
		}
	})

	select {
	case <-sig.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the write to enter the wrapped writer")
	}

	stopErr := make(chan error, 1)
	go func() {
		stopErr <- (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state})
	}()

	select {
	case err := <-stopErr:
		if err != nil {
			t.Errorf("StopSession() error = %v, want nil", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StopSession() did not return while a write was parked, want it to return well short of its own 5-second graceful-wait ceiling")
	}
}

// startFakeCodexProcess writes scriptBody, touching a readiness marker
// right after its leading trap statement so a caller's subsequent
// SignalGraceful cannot race the shell installing the trap, starts it
// in its own process group, and wires a minimal sessionState around it
// with an already-closed readerDone (this harness has no reader
// goroutine for StopSession to wait for).
func startFakeCodexProcess(t *testing.T, scriptBody string, stopGraceMS int) *sessionState {
	t.Helper()

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	trapLine, rest, ok := strings.Cut(scriptBody, "\n")
	if !ok || !strings.HasPrefix(trapLine, "trap ") {
		t.Fatalf("startFakeCodexProcess: scriptBody must start with a trap statement, got %q", scriptBody)
	}
	script := trapLine + "\ntouch '" + readyPath + "'\n" + rest
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", script)

	cmd := exec.Command(scriptPath) //nolint:gosec // fixed path under t.TempDir()
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	t.Cleanup(func() { procutil.KillProcessGroup(cmd.Process.Pid) }) //nolint:errcheck // best-effort cleanup

	readerDone := make(chan struct{})
	close(readerDone)

	waitCh := make(chan struct{})
	state := &sessionState{
		agentConfig: domain.AgentConfig{StopGraceMS: stopGraceMS},
		proc:        cmd.Process,
		waitCh:      waitCh,
		readerDone:  readerDone,
	}
	go func() {
		cmd.Wait() //nolint:errcheck,gosec // best-effort reap; exit state is irrelevant here
		close(waitCh)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("startFakeCodexProcess: readiness marker %q did not appear within 5s", readyPath)
	return nil
}

// TestStopSession_ConfiguredGraceBoundsTheWait asserts that a
// configured agent.stop_grace_ms bounds StopSession's graceful wait,
// not the built-in five-second default.
func TestStopSession_ConfiguredGraceBoundsTheWait(t *testing.T) {
	t.Parallel()

	state := startFakeCodexProcess(t, `trap '' TERM
while :; do :; done`, 200)

	start := time.Now()
	err := (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state})
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("StopSession() = %v, want nil", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("StopSession() force-terminated after %v, want well under the built-in 5s default (proves the configured 200ms grace bounded the wait, not DefaultStopGrace)", elapsed)
	}
}

// TestStopSession_EscalationLogging asserts the escalation records
// codex's StopSession emits: Debug on an exit inside the grace, and
// Warn naming the outcome, the configured ceiling and the elapsed wait
// when the phase ends without one. Both escalation outcomes are
// reachable here, because StopSession ends the phase on whichever of
// the grace and the caller's deadline arrives first.
func TestStopSession_EscalationLogging(t *testing.T) {
	// No t.Parallel(): installs a global slog default.

	t.Run("exit_inside_grace_emits_debug_and_no_warn", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(orig) })

		// The grace is far longer than this exit needs. The subtest proves
		// which record the clean-exit path emits, not that the grace bounds
		// anything, and a tight bound races the runner's scheduler instead
		// of testing the code.
		state := startFakeCodexProcess(t, `trap 'exit 0' TERM
while :; do :; done`, 30000)

		if err := (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state}); err != nil {
			t.Errorf("StopSession() = %v, want nil", err)
		}

		output := buf.String()
		if !strings.Contains(output, "agent exited during the graceful phase") {
			t.Errorf("StopSession() did not log the exited-inside-grace Debug record: %s", output)
		}
		if !strings.Contains(output, `outcome=exited`) {
			t.Errorf("StopSession()'s Debug record missing outcome=exited: %s", output)
		}
		if strings.Contains(output, "level=WARN") {
			t.Errorf("StopSession() logged a Warn record for a clean exit, want none: %s", output)
		}
	})

	t.Run("grace_elapsed_emits_warn_with_outcome_and_grace", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(orig) })

		state := startFakeCodexProcess(t, `trap '' TERM
while :; do :; done`, 150)

		if err := (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state}); err != nil {
			t.Errorf("StopSession() = %v, want nil", err)
		}

		output := buf.String()
		if !strings.Contains(output, "agent did not exit inside the graceful period and was force-terminated") {
			t.Errorf("StopSession() did not log the grace-elapsed Warn record: %s", output)
		}
		if !strings.Contains(output, `outcome="grace elapsed"`) {
			t.Errorf(`StopSession()'s Warn record missing outcome="grace elapsed": %s`, output)
		}
		if !strings.Contains(output, "grace=") {
			t.Errorf("StopSession()'s Warn record missing the configured grace ceiling: %s", output)
		}
		if !strings.Contains(output, "elapsed=") {
			t.Errorf("StopSession()'s Warn record missing the elapsed wait: %s", output)
		}
	})

	t.Run("caller_deadline_ends_the_phase_and_is_reported", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(orig) })

		// The grace is far longer than the deadline, so only a
		// StopSession that reads its context can end this phase. When
		// it ignored the context, this arm waited out the whole grace
		// and then reported success.
		state := startFakeCodexProcess(t, `trap '' TERM
while :; do :; done`, 30000)

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()

		start := time.Now()
		err := (&CodexAdapter{}).StopSession(ctx, domain.Session{Internal: state})
		elapsed := time.Since(start)

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("StopSession() = %v, want context.DeadlineExceeded", err)
		}
		if elapsed > 10*time.Second {
			t.Errorf("StopSession() returned after %v, want the caller's deadline to end the phase far below the 30s grace", elapsed)
		}

		output := buf.String()
		if !strings.Contains(output, `outcome="caller deadline"`) {
			t.Errorf(`StopSession()'s Warn record missing outcome="caller deadline": %s`, output)
		}
		if !strings.Contains(output, "grace=30s") {
			t.Errorf("StopSession()'s Warn record did not report the configured 30s ceiling: %s", output)
		}
	})
}

// handlerParkedOutcome is what a test's own goroutine running RunTurn
// sends back once that call returns.
type handlerParkedOutcome struct {
	result domain.TurnResult
	err    error
}

// fakeScenarioEnv names the fake app-server scenario a re-executed copy
// of this test binary must serve. TestMain switches on it before the
// package's own tests run, so a protocol fixture needs no external
// interpreter and stays ordinary, debuggable Go.
const fakeScenarioEnv = "SORTIE_TEST_CODEX_FAKE_SCENARIO"

const (
	// scenarioBurstDuringTurnOpening answers the handshake, then on
	// turn/start writes a burst in one write, the turn/start response,
	// and turn/completed, and keeps reading its own standard input: the
	// runtime stays alive and healthy throughout.
	scenarioBurstDuringTurnOpening = "burst-turn-opening"

	// scenarioBurstBetweenTurns completes one turn normally, then writes
	// a burst in one write, creates a marker file at the path named by
	// burstBetweenTurnsMarkerEnv, and waits for the next turn/start
	// before answering it.
	scenarioBurstBetweenTurns = "burst-between-turns"

	// scenarioHandshakeBurstLogin writes a burst, then
	// account/login/completed, before ever answering account/login/start.
	scenarioHandshakeBurstLogin = "handshake-burst-login"

	// scenarioHandshakeBurstThread writes a burst, then thread/started,
	// before ever answering thread/start.
	scenarioHandshakeBurstThread = "handshake-burst-thread"
)

// burstCount is a burst of at least 4096 notifications totaling at
// least 1 MiB, written in one write. Any bounded hand-off parks on it,
// and it fills every platform's pipe buffer.
const burstCount = 27000

// burstBetweenTurnsMarkerEnv names the environment variable
// scenarioBurstBetweenTurns reads the marker file path from. The
// adapter launches its runtime with the parent's own environment, so
// setting it in the test process reaches the re-executed fake
// app-server.
const burstBetweenTurnsMarkerEnv = "SORTIE_TEST_CODEX_BURST_MARKER_PATH"

func TestMain(m *testing.M) {
	if scenario := os.Getenv(fakeScenarioEnv); scenario != "" {
		os.Exit(serveFakeAppServer(scenario, os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

// fakeAppServer points the adapter at this test binary, which TestMain
// re-enters as the named scenario's fake app-server. The scenario
// travels in the environment because the adapter launches its runtime
// with the parent's own environment.
func fakeAppServer(t *testing.T, scenario string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	t.Setenv(fakeScenarioEnv, scenario)
	return self
}

// fakeFrame is the part of an incoming JSON-RPC frame a scenario acts
// on: the id it echoes back in a reply, absent on a notification.
type fakeFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

// serveFakeAppServer runs one scenario against the client on in and out
// and returns the process exit code. A scenario whose input ends before
// its sequence completes exits non-zero, which the adapter sees as the
// runtime dying rather than as silence.
func serveFakeAppServer(scenario string, in io.Reader, out io.Writer) int {
	client := bufio.NewScanner(in)
	client.Buffer(make([]byte, 0, 4096), 1024*1024)

	switch scenario {
	case scenarioBurstDuringTurnOpening:
		return serveBurstDuringTurnOpening(client, out)
	case scenarioBurstBetweenTurns:
		return serveBurstBetweenTurns(client, out)
	case scenarioHandshakeBurstLogin:
		return serveHandshakeBurstLogin(client, out)
	case scenarioHandshakeBurstThread:
		return serveHandshakeBurstThread(client, out)
	default:
		fmt.Fprintf(os.Stderr, "fake app-server: unknown scenario %q\n", scenario)
		return 2
	}
}

// nextFrame reports the client's next frame, or false once the client
// has stopped sending.
func nextFrame(client *bufio.Scanner) (fakeFrame, bool) {
	if !client.Scan() {
		return fakeFrame{}, false
	}
	var frame fakeFrame
	if err := json.Unmarshal(client.Bytes(), &frame); err != nil {
		return fakeFrame{}, false
	}
	return frame, true
}

// writeFrame writes one frame to the client, reporting whether the
// client is still reading it.
func writeFrame(out io.Writer, format string, args ...any) bool {
	_, err := fmt.Fprintf(out, format, args...)
	return err == nil
}

// answerPreThreadHandshake replies to initialize and to account/read,
// consuming the initialized notification between them: the exchange
// every scenario shares before it diverges.
func answerPreThreadHandshake(client *bufio.Scanner, out io.Writer) bool {
	initialize, ok := nextFrame(client)
	if !ok {
		return false
	}
	if !writeFrame(out, "{\"id\":%s,\"result\":{}}\n", initialize.ID) {
		return false
	}

	if _, ok := nextFrame(client); !ok {
		return false
	}

	accountRead, ok := nextFrame(client)
	if !ok {
		return false
	}
	return writeFrame(out, "{\"id\":%s,\"result\":{}}\n", accountRead.ID)
}

// fillerNotifications returns burstCount notifications as one string,
// so a caller delivers the whole burst in a single write the way a
// runtime flooding its output does.
func fillerNotifications() string {
	var fill strings.Builder
	for i := range burstCount {
		fmt.Fprintf(&fill, "{\"method\":\"filler/notification\",\"params\":{\"i\":%d}}\n", i)
	}
	return fill.String()
}

// serveBurstDuringTurnOpening answers the handshake and thread/start,
// then on the turn/start call writes burstCount notifications in one
// write, followed by the turn/start response and turn/completed, and
// keeps reading its own standard input rather than exiting: the
// runtime stays alive and healthy for the whole exchange.
func serveBurstDuringTurnOpening(client *bufio.Scanner, out io.Writer) int {
	if !answerPreThreadHandshake(client, out) {
		return 1
	}
	threadStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeFrame(out, "{\"id\":%s,\"result\":{\"thread\":{\"id\":\"fake-thread-1\"}}}\n", threadStart.ID) {
		return 1
	}
	if !writeFrame(out, "{\"method\":\"thread/started\",\"params\":{}}\n") {
		return 1
	}

	turnStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if _, err := io.WriteString(out, fillerNotifications()); err != nil {
		return 1
	}
	if !writeFrame(out, "{\"id\":%s,\"result\":{\"turn\":{\"id\":\"t1\"}}}\n", turnStart.ID) {
		return 1
	}
	if !writeFrame(out, "{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"t1\",\"status\":\"completed\"}}}\n") {
		return 1
	}

	for client.Scan() {
	}
	return 0
}

// serveBurstBetweenTurns answers the handshake, thread/start, and one
// full turn, then writes burstCount notifications in one write,
// creates a marker file at the path burstBetweenTurnsMarkerEnv names,
// and waits for the next turn/start before answering it and keeping its
// own standard input open.
func serveBurstBetweenTurns(client *bufio.Scanner, out io.Writer) int {
	if !answerPreThreadHandshake(client, out) {
		return 1
	}
	threadStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeFrame(out, "{\"id\":%s,\"result\":{\"thread\":{\"id\":\"fake-thread-1\"}}}\n", threadStart.ID) {
		return 1
	}
	if !writeFrame(out, "{\"method\":\"thread/started\",\"params\":{}}\n") {
		return 1
	}

	turnStart1, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeFrame(out, "{\"id\":%s,\"result\":{\"turn\":{\"id\":\"t1\"}}}\n", turnStart1.ID) {
		return 1
	}
	if !writeFrame(out, "{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"t1\",\"status\":\"completed\"}}}\n") {
		return 1
	}

	if _, err := io.WriteString(out, fillerNotifications()); err != nil {
		return 1
	}
	if markerPath := os.Getenv(burstBetweenTurnsMarkerEnv); markerPath != "" {
		if err := os.WriteFile(markerPath, []byte("done"), 0o644); err != nil {
			return 1
		}
	}

	turnStart2, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeFrame(out, "{\"id\":%s,\"result\":{\"turn\":{\"id\":\"t2\"}}}\n", turnStart2.ID) {
		return 1
	}
	if !writeFrame(out, "{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"t2\",\"status\":\"completed\"}}}\n") {
		return 1
	}

	for client.Scan() {
	}
	return 0
}

// serveHandshakeBurstLogin answers initialize and account/read, then on
// account/login/start writes burstCount notifications in one write,
// followed by account/login/completed reporting success, and only then
// answers the account/login/start call itself.
func serveHandshakeBurstLogin(client *bufio.Scanner, out io.Writer) int {
	if !answerPreThreadHandshake(client, out) {
		return 1
	}
	loginStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if _, err := io.WriteString(out, fillerNotifications()); err != nil {
		return 1
	}
	if !writeFrame(out, "{\"method\":\"account/login/completed\",\"params\":{\"success\":true}}\n") {
		return 1
	}
	if !writeFrame(out, "{\"id\":%s,\"result\":{}}\n", loginStart.ID) {
		return 1
	}

	threadStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeFrame(out, "{\"id\":%s,\"result\":{\"thread\":{\"id\":\"fake-thread-1\"}}}\n", threadStart.ID) {
		return 1
	}
	if !writeFrame(out, "{\"method\":\"thread/started\",\"params\":{}}\n") {
		return 1
	}

	for client.Scan() {
	}
	return 0
}

// serveHandshakeBurstThread answers the handshake, then on thread/start
// writes burstCount notifications in one write, followed by
// thread/started, and only then answers the thread/start call itself.
func serveHandshakeBurstThread(client *bufio.Scanner, out io.Writer) int {
	if !answerPreThreadHandshake(client, out) {
		return 1
	}
	threadStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if _, err := io.WriteString(out, fillerNotifications()); err != nil {
		return 1
	}
	if !writeFrame(out, "{\"method\":\"thread/started\",\"params\":{}}\n") {
		return 1
	}
	if !writeFrame(out, "{\"id\":%s,\"result\":{\"thread\":{\"id\":\"fake-thread-1\"}}}\n", threadStart.ID) {
		return 1
	}

	for client.Scan() {
	}
	return 0
}

// waitChOpen reports whether state.waitCh is still open, without
// blocking.
func waitChOpen(state *sessionState) bool {
	select {
	case <-state.waitCh:
		return false
	default:
		return true
	}
}

// countOtherMessages reports how many events events carries whose
// Type is domain.EventOtherMessage and whose Message names method.
func countOtherMessages(events []domain.AgentEvent, method string) int {
	var n int
	for _, e := range events {
		if e.Type == domain.EventOtherMessage && e.Message == method {
			n++
		}
	}
	return n
}

// TestRunTurn_BurstDuringTurnOpeningNoLongerHangs drives a fake runtime
// that stays alive throughout and, on turn/start, writes burstCount
// notifications in one write before the turn/start response and
// turn/completed. RunTurn must complete rather than wait on the
// orchestrator's own stall or turn timeout, with the runtime's own
// process still unreaped throughout.
// Not run with t.Parallel(): fakeAppServer uses t.Setenv.
func TestRunTurn_BurstDuringTurnOpeningNoLongerHangs(t *testing.T) {
	command := fakeAppServer(t, scenarioBurstDuringTurnOpening)
	adapter := &CodexAdapter{}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: command},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	var events []domain.AgentEvent
	outcomeCh := make(chan handlerParkedOutcome, 1)
	go func() {
		result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "work",
			OnEvent: collectEvents(&events),
		})
		outcomeCh <- handlerParkedOutcome{result: result, err: runErr}
	}()

	var got handlerParkedOutcome
	select {
	case got = <-outcomeCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("RunTurn has not returned after 10s, state.waitCh still open = %v", waitChOpen(state))
	}

	if got.err != nil {
		t.Errorf("RunTurn() error = %v, want nil", got.err)
	}
	if got.result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn() ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCompleted)
	}
	if !waitChOpen(state) {
		t.Error("state.waitCh is closed, want the runtime still alive and unreaped")
	}
	if n := countOtherMessages(events, "filler/notification"); n != burstCount {
		t.Errorf("filler notification events = %d, want %d", n, burstCount)
	}
}

// TestRunTurn_BurstBetweenTurnsNoLongerHangs drives a fake runtime that
// completes one turn normally, then, with no turn in flight, writes
// burstCount notifications in one write and creates a marker file
// before waiting for the next turn/start. The marker must appear well
// before the second turn is even started, and the second turn must then
// satisfy the same properties as the turn-opening burst.
// Not run with t.Parallel(): uses t.Setenv and fakeAppServer.
func TestRunTurn_BurstBetweenTurnsNoLongerHangs(t *testing.T) {
	dir := t.TempDir()
	markerPath := filepath.Join(dir, "marker")
	t.Setenv(burstBetweenTurnsMarkerEnv, markerPath)

	command := fakeAppServer(t, scenarioBurstBetweenTurns)
	adapter := &CodexAdapter{}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: command},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	result1, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "first",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(1) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn(1) ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(markerPath); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker file is absent after 10s, state.waitCh still open = %v", waitChOpen(state))
		}
		time.Sleep(5 * time.Millisecond)
	}

	var events []domain.AgentEvent
	outcomeCh := make(chan handlerParkedOutcome, 1)
	go func() {
		result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "second",
			OnEvent: collectEvents(&events),
		})
		outcomeCh <- handlerParkedOutcome{result: result, err: runErr}
	}()

	var got handlerParkedOutcome
	select {
	case got = <-outcomeCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("RunTurn(2) has not returned after 10s, state.waitCh still open = %v", waitChOpen(state))
	}

	if got.err != nil {
		t.Errorf("RunTurn(2) error = %v, want nil", got.err)
	}
	if got.result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(2) ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCompleted)
	}
	if !waitChOpen(state) {
		t.Error("state.waitCh is closed, want the runtime still alive and unreaped")
	}
	if n := countOtherMessages(events, "filler/notification"); n != burstCount {
		t.Errorf("filler notification events = %d, want %d", n, burstCount)
	}
}

// TestStartSession_HandshakeBurstDoesNotLoseAwaitedNotification drives
// two fake runtimes, each of which writes burstCount notifications in
// one write before the notification the handshake waits for: the first
// answers account/login/start after account/login/completed, with
// CODEX_API_KEY set; the second answers thread/start after
// thread/started. Each StartSession must succeed within 2s: the burst
// must not cost the handshake the notification it is waiting for.
func TestStartSession_HandshakeBurstDoesNotLoseAwaitedNotification(t *testing.T) {
	tests := []struct {
		name     string
		scenario string
		apiKey   string
	}{
		{
			name:     "account/login/completed observed despite the burst",
			scenario: scenarioHandshakeBurstLogin,
			apiKey:   "test-api-key",
		},
		{
			name:     "thread/started observed despite the burst",
			scenario: scenarioHandshakeBurstThread,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CODEX_API_KEY", tt.apiKey)

			command := fakeAppServer(t, tt.scenario)
			adapter := &CodexAdapter{}

			start := time.Now()
			session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath: t.TempDir(),
				AgentConfig: domain.AgentConfig{
					Command:       command,
					ReadTimeoutMS: 5000,
				},
			})
			elapsed := time.Since(start)

			if err != nil {
				var agentErr *domain.AgentError
				if errors.As(err, &agentErr) && agentErr.Kind == domain.ErrResponseError && strings.Contains(agentErr.Message, "account/login/completed") {
					t.Fatalf("StartSession() lost the awaited notification to the burst and failed authentication: %v", err)
				}
				t.Fatalf("StartSession() error = %v, want nil", err)
			}
			t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

			if elapsed > 2*time.Second {
				t.Fatalf("StartSession() took %v past its 2s bound, want the burst to cost it nothing", elapsed)
			}
		})
	}
}
