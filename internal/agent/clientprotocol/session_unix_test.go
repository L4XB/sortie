//go:build unix

package clientprotocol

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// mcpHandshakeScript is a fake agent that answers exactly the two
// calls startSession makes before returning: initialize (always id 1,
// since it is the connection's first call) and session/new (always id
// 2). It captures the raw session/new request line to captureFile
// before answering it, so a test can inspect the exact bytes the
// adapter wrote for its own tool-server delivery.
func mcpHandshakeScript(captureFile string) string {
	return `capture='` + captureFile + `'
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true}}}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' "$line" >> "$capture"
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      ;;
  esac
done
`
}

// TestStartSessionMCPInjectionWire calls agenttest.AssertMCPInjection
// with the wire channel populated from the actual session/new request
// bytes the adapter wrote, and is what gives cmd/sortie's
// TestEveryAgentKindHasMCPInjectionCoverage a call site for this
// kind. It launches a shell-script agent, so it runs only where
// /bin/sh exists.
func TestStartSessionMCPInjectionWire(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mcpConfigPath := writeMCPConfig(t)
	capturePath := filepath.Join(dir, "session_new.jsonl")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeScript(capturePath))

	params := domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
		MCPConfigPath: mcpConfigPath,
	}

	session, err := startSession(context.Background(), &ClientProtocolAdapter{}, params)
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("stopSession() error = %v", err)
		}
	})

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured session/new request: %v", err)
	}

	declared, ok := registry.Agents.Meta("agent-client-protocol")
	if !ok {
		t.Fatal(`registry.Agents.Meta("agent-client-protocol") ok = false, want true`)
	}

	agenttest.AssertMCPInjection(t, declared.MCPInjection, mcpConfigPath, agenttest.MCPLaunchSurface{
		Wire: []string{strings.TrimSpace(string(captured))},
	})
}

// mcpHandshakeThenGracefulExitScript installs a handler for the
// graceful signal that waits delaySeconds and writes evidencePath
// before exiting, then answers startSession's handshake exactly as
// mcpHandshakeScript does, so a test can tell a catchable signal from
// an uncatchable kill.
//
// The handler is installed before the handshake rather than after it.
// A caller signals only once startSession has returned, which cannot
// happen until the handshake below has been answered, so installing
// first leaves no window in which the default disposition applies and
// the evidence is never written. The idle loop sleeps in short
// intervals because the handler does not run until the command it
// interrupts has finished.
func mcpHandshakeThenGracefulExitScript(evidencePath, delaySeconds string) string {
	return `trap 'sleep ` + delaySeconds + `; touch "` + evidencePath + `"; exit 0' TERM
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      break
      ;;
  esac
done
while :; do sleep 0.05; done
`
}

// TestStartSessionCancelledLaunchContextSignalsGracefully:
// cancelling the context a session was started with, without calling
// stopSession at all, delivers the catchable termination signal to the
// agent rather than an uncatchable kill, and the agent's handler
// evidence shows it. The property fails if either startSession's
// cmd.Cancel or its cmd.WaitDelay is removed: without them,
// exec.CommandContext's default cancellation path SIGKILLs the direct
// child instead, the handler this fixture depends on never runs, and
// the evidence file this test waits for never appears.
func TestStartSessionCancelledLaunchContextSignalsGracefully(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	evidencePath := filepath.Join(dir, "evidence")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeThenGracefulExitScript(evidencePath, "0.4"))

	ctx, cancel := context.WithCancel(context.Background())
	session, err := startSession(ctx, &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("stopSession() error = %v", err)
		}
	})

	cancel()
	waitForFile(t, evidencePath)
}

// stderrThenExitScript is a fake agent that writes marker to stderr
// and exits immediately without reading or answering anything on
// stdin, so startSession's initialize call fails against a closed
// connection rather than a timeout.
func stderrThenExitScript(marker string) string {
	return `printf '%s\n' '` + marker + `' 1>&2
exit 1
`
}

// TestStartSessionEmitsCollectedStderrAtWarnOnFailedInitialize confirms
// that a fake agent which exits before answering initialize still gets
// its collected stderr surfaced at Warn before startSession returns its
// error. The test fails if the EmitWarnLines call following
// doInitialize's failure branch is removed: the returned error would be
// unaffected, but the marker line would never reach the logger.
func TestStartSessionEmitsCollectedStderrAtWarnOnFailedInitialize(t *testing.T) {
	// No t.Parallel(): installs a process-wide slog default.

	const marker = "distinguishing-stderr-line-init-failure"

	dir := t.TempDir()
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", stderrThenExitScript(marker))

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	_, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})

	if _, ok := errors.AsType[*domain.AgentError](err); !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}

	output := buf.String()
	if !strings.Contains(output, marker) {
		t.Errorf("startSession() on failed initialize did not log the collected stderr line %q at Warn: %s", marker, output)
	}
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("startSession()'s collected stderr line was logged below Warn: %s", output)
	}
}

// mcpHandshakeWithDetachedChildScript answers the handshake exactly as
// mcpHandshakeScript does, but first backgrounds a helper process
// under setsid: a new session and process group leader, escaping the
// group procutil.SetGroupCancel placed this script's own process in.
// The helper records its own pid to pidFile and idles, so a test can
// wait for it to exist and then check its liveness directly.
func mcpHandshakeWithDetachedChildScript(pidFile string) string {
	return mcpHandshakeWithChildScript("setsid ", pidFile)
}

// mcpHandshakeWithGroupChildScript answers the handshake and leaves one
// idle child inside the process group procutil.SetGroupCancel placed
// this script in, so teardown's group-directed termination reaches it.
func mcpHandshakeWithGroupChildScript(pidFile string) string {
	return mcpHandshakeWithChildScript("", pidFile)
}

// mcpHandshakeWithChildScript builds both of the above. launcher
// prefixes the child's own command and is what decides whether the
// child keeps the script's process group or leaves it.
func mcpHandshakeWithChildScript(launcher, pidFile string) string {
	return launcher + `sh -c 'echo $$ >"` + pidFile + `"; while :; do sleep 0.05; done' </dev/null >/dev/null 2>&1 &
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      break
      ;;
  esac
done
while :; do sleep 0.05; done
`
}

// killLaunchedGroup signals the process group the launched agent leads,
// tolerating a pid that has already gone. A child that stayed in that
// group leads no group of its own, so a kill aimed at the child's own
// pid reaches nothing; killHelperGroup is right only for the detached
// child below, which setsid makes a leader.
func killLaunchedGroup(agentPID string) {
	pid, err := strconv.Atoi(strings.TrimSpace(agentPID))
	if err != nil || pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// initThenStderrExitScript answers initialize, writes marker to stderr,
// and exits before answering session/new, so startSession's failure
// arrives from resolveSession rather than from doInitialize.
func initThenStderrExitScript(marker string) string {
	return `while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      printf '%s\n' '` + marker + `' 1>&2
      exit 1
      ;;
  esac
done
`
}

// TestStartSessionEmitsCollectedStderrAtWarnOnFailedResolveSession
// mirrors the initialize-failure case above for the resolve-session
// failure branch. startSession emits collected stderr on two failure
// branches, and a control that reaches only the first lets the
// second's emission be deleted without a test going red.
func TestStartSessionEmitsCollectedStderrAtWarnOnFailedResolveSession(t *testing.T) {
	// No t.Parallel(): installs a process-wide slog default.

	const marker = "distinguishing-stderr-line-resolve-failure"

	dir := t.TempDir()
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", initThenStderrExitScript(marker))

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	_, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})

	if _, ok := errors.AsType[*domain.AgentError](err); !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}

	output := buf.String()
	if !strings.Contains(output, marker) {
		t.Errorf("startSession() on failed resolveSession did not log the collected stderr line %q at Warn: %s", marker, output)
	}
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("startSession()'s collected stderr line was logged below Warn: %s", output)
	}
}

// TestStopSessionReachesGroupChild is the other half of the property
// the escaped-member test below evidences. Teardown does reach a
// descendant that stays in the group it was launched into, so a
// survivor outside that group means the group was escaped rather than
// that teardown reaches nothing at all. Removing procutil.SetGroupCancel
// from startSession fails this test and not that one.
func TestStopSessionReachesGroupChild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "group-child.pid")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeWithGroupChildScript(pidPath))

	session, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}

	childPID := waitForPIDFile(t, pidPath, awaitTimeout)
	t.Cleanup(func() { killLaunchedGroup(session.AgentPID) })

	if err := stopSession(context.Background(), session); err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}

	assertProcessGone(t, childPID, awaitTimeout)
}

// TestStopSessionDoesNotReachEscapedProcessGroupMember confirms that a
// descendant which detaches into its own process group before the
// parent exits survives stopSession's group-directed termination, so a
// leaked survivor is demonstrably detectable by a direct
// process-liveness check rather than merely assumed. This is not a
// defect in stopSession; procutil.SetGroupCancel
// and kill_process_group can only ever reach the group they targeted.
func TestStopSessionDoesNotReachEscapedProcessGroupMember(t *testing.T) {
	t.Parallel()
	agenttest.RequireSetsid(t)

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeWithDetachedChildScript(pidPath))

	session, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}

	escapedPID := waitForPIDFile(t, pidPath, awaitTimeout)
	t.Cleanup(func() { killHelperGroup(pidPath) })

	if err := stopSession(context.Background(), session); err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}

	if err := syscall.Kill(escapedPID, 0); err != nil {
		t.Errorf("escaped process group member (pid %d) liveness probe after stopSession() = %v, want still running", escapedPID, err)
	}
}

// stderrThenExitWithDetachedHolderScript writes marker to standard
// error, backgrounds a setsid descendant that inherits this script's
// own standard-output handle (a plain shell background job keeps the
// parent's file descriptors unless it redirects them, and setsid is
// what lets the descendant survive teardown's group-directed kill),
// then exits before ever reading or answering the initialize call. The
// runtime is gone, but the escaped descendant keeps the output pipe
// from reaching end of file, so a handshake call is still in flight
// when the release gives up on it.
func stderrThenExitWithDetachedHolderScript(marker, pidFile string) string {
	return `setsid sh -c 'echo $$ > ` + pidFile + `; sleep 3600' 2>/dev/null &
while [ ! -s ` + pidFile + ` ]; do sleep 0.01; done
printf '%s\n' '` + marker + `' 1>&2
exit 7
`
}

// TestStartSessionHandshakeAbandonedByEscapedDescendantFailsWithPortExit
// asserts that a runtime that exits during the handshake while an
// escaped descendant still holds the output handle fails StartSession
// at the reap plus the injected grace with domain.ErrPortExit, and the
// runtime's own stderr line still reaches the operator.
//
// No t.Parallel(): installs a process-wide slog default.
func TestStartSessionHandshakeAbandonedByEscapedDescendantFailsWithPortExit(t *testing.T) {
	agenttest.RequireSetsid(t)

	const marker = "distinguishing-stderr-line-handshake-abandoned"
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	t.Cleanup(func() { killHelperGroup(pidPath) })
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", stderrThenExitWithDetachedHolderScript(marker, pidPath))

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	const grace = 200 * time.Millisecond
	adapter := &ClientProtocolAdapter{drainGrace: grace}

	start := time.Now()
	_, err := startSession(context.Background(), adapter, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	elapsed := time.Since(start)

	agentErr, ok := errors.AsType[*domain.AgentError](err)
	if !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("startSession() error kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	const wantMessage = "agent connection ended before responding"
	if agentErr.Message != wantMessage {
		t.Errorf("startSession() error message = %q, want %q", agentErr.Message, wantMessage)
	}
	if elapsed > 10*time.Second {
		t.Errorf("startSession() took %v, want well under the 30s handshake timeout it would have hit without the release", elapsed)
	}

	output := buf.String()
	if !hasWarnStderrLineRecord(output, marker) {
		t.Errorf("startSession() output = %s, want one record carrying level=WARN, msg=\"agent stderr\", and line=%s together (the collector also logs this same line at Debug, which must not satisfy this check on its own)", output, marker)
	}
}

// hasWarnStderrLineRecord reports whether output contains one log
// record, on a single line, that carries level=WARN, msg="agent
// stderr", and line=marker together: the three fields a
// procutil.EmitWarnLines call produces for a collected stderr line.
// The collector's own Debug-level record for the same line carries
// the same message and line fields but a different level, so the
// three checks must hold on one record rather than anywhere in the
// buffer.
func hasWarnStderrLineRecord(output, marker string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "level=WARN") &&
			strings.Contains(line, `msg="agent stderr"`) &&
			strings.Contains(line, "line="+marker) {
			return true
		}
	}
	return false
}

// handshakeThenExitWithDetachedHolderScript answers the two calls
// startSession makes before returning, then spawns a setsid
// descendant that inherits this script's own standard-output handle
// and exits without waiting for it, so a real subprocess reproduces
// "the runtime exits, its descendant survives holding the write end"
// for a session that has already started.
func handshakeThenExitWithDetachedHolderScript(pidFile string) string {
	return `while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      break
      ;;
  esac
done
setsid sh -c 'echo $$ > ` + pidFile + `; sleep 3600' 2>/dev/null &
while [ ! -s ` + pidFile + ` ]; do sleep 0.01; done
exit 0
`
}

// TestStopSessionReturnsBoundedAfterReleaseAbandonsOnARealSubprocess
// asserts that, with the runtime gone and an escaped descendant still
// holding the standard-output handle, once the release has abandoned,
// StopSession still returns inside its pinned teardown ceiling against
// a real subprocess. On Linux the release's own CloseStdout already
// unparks the connection's reader by the time this runs, so this alone
// does not exercise the post-abandonment stop arm runPump's select
// falls back to when a reader stays genuinely parked; that arm is
// proven separately, by an untagged test whose pipes are never wired
// to the connection under test.
func TestStopSessionReturnsBoundedAfterReleaseAbandonsOnARealSubprocess(t *testing.T) {
	t.Parallel()
	agenttest.RequireSetsid(t)

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	t.Cleanup(func() { killHelperGroup(pidPath) })
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", handshakeThenExitWithDetachedHolderScript(pidPath))

	const grace = 200 * time.Millisecond
	adapter := &ClientProtocolAdapter{drainGrace: grace}
	session, err := startSession(context.Background(), adapter, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v, want nil", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}

	select {
	case <-state.release.Abandoned():
	case <-time.After(5 * time.Second):
		t.Fatal("the release never abandoned the parked reader")
	}

	start := time.Now()
	if err := stopSession(context.Background(), session); err != nil {
		t.Errorf("stopSession() error = %v, want nil", err)
	}
	elapsed := time.Since(start)

	ceiling := procutil.DefaultStopGrace + 3*procutil.DefaultDrainGrace + teardownReturnOverhead
	if elapsed >= ceiling {
		t.Errorf("stopSession() took %v, want under %v (the pinned teardown ceiling)", elapsed, ceiling)
	}
}
