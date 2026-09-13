package jsonrpc_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
)

// childEnv, when set to "1" in the test binary's own environment,
// tells the re-executed copy of this binary to run as the fake
// runtime this file's tests drive, instead of running the package's
// own tests. This is the same re-executed-test-binary pattern
// internal/agent/codex/codex_test.go's TestMain and
// internal/agent/procutil/pgid_test.go use, chosen because it needs no
// external interpreter and so runs unchanged on every platform this
// project builds for, including Windows.
const childEnv = "SORTIE_TEST_JSONRPC_SUBPROCESS_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		// The fake runtime this test drives: it stays alive and never
		// reads its own standard input, so a write behind it either
		// enqueues and returns (per this package's contract) or parks in
		// the underlying OS pipe until this process is killed. A
		// receive from an unbuffered channel nobody ever sends on would
		// read the same on paper, but with no goroutine and no pending
		// timer left anywhere in the process, the runtime's own
		// deadlock detector treats that as every goroutine being
		// permanently asleep and kills the process with "fatal error:
		// all goroutines are asleep - deadlock!" before the parent test
		// ever gets to exercise it. Sleeping registers a timer, so the
		// detector never fires.
		time.Sleep(24 * time.Hour)
		return
	}
	os.Exit(m.Run())
}

// startNeverReadingChild launches a copy of this test binary as the
// fake runtime childEnv selects, wires w and r to its standard input
// and standard output, and returns them along with a release func that
// kills and reaps the child. The child's standard output is never
// written to; it exists only so jsonrpc.NewConn has a reader to start
// against.
func startNeverReadingChild(t *testing.T) (w io.WriteCloser, r io.Reader, releaseChild func()) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}

	cmd := exec.Command(self) //nolint:gosec // re-executes this test binary, not an arbitrary path
	cmd.Env = append(os.Environ(), childEnv+"=1")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("cmd.StdinPipe() error = %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("cmd.StdoutPipe() error = %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() error = %v", err)
	}

	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()

	release := func() {
		_ = cmd.Process.Kill()
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
		}
	}
	return stdin, stdout, release
}

// waitForGoroutineCountAtOrBelow polls runtime.NumGoroutine() until it
// settles at or below want, or fails t once timeout elapses. A single
// sample taken right after the goroutines of interest should have
// exited is not reliable: the runtime schedules their actual exit
// slightly after the channel or process event a test observes to learn
// they are done.
func waitForGoroutineCountAtOrBelow(t *testing.T, want int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if n := runtime.NumGoroutine(); n <= want {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("runtime.NumGoroutine() = %d after waiting %v, want <= %d (a goroutine leaked)", runtime.NumGoroutine(), timeout, want)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestConn_RealSubprocessNeverReadingStdin drives a real child process,
// re-executed from this test binary, that stays alive and never reads
// its own standard input. Against that child: a large Notify returns
// at once, Flush under a short ctx times out because the write behind
// it is genuinely parked in the OS pipe, closing the write end makes
// WriteFailed close within a bound, and no goroutine this test's own
// connection started survives cleanup.
func TestConn_RealSubprocessNeverReadingStdin(t *testing.T) {
	baseline := runtime.NumGoroutine()

	stdin, stdout, releaseChild := startNeverReadingChild(t)
	t.Cleanup(releaseChild)

	conn := jsonrpc.NewConn(stdin, stdout, discardSink())

	// A payload well past any platform's OS pipe buffer, so the
	// underlying Write this enqueues cannot complete until either the
	// child reads it (it never does) or the pipe is closed.
	largeParams := map[string]any{"blob": strings.Repeat("x", 8<<20)}

	notifyDone := make(chan error, 1)
	notifyStart := time.Now()
	go func() { notifyDone <- conn.Notify("large/notify", largeParams) }()

	select {
	case err := <-notifyDone:
		if err != nil {
			t.Fatalf("Notify() error = %v", err)
		}
		if elapsed := time.Since(notifyStart); elapsed > 2*time.Second {
			t.Errorf("Notify() took %v to return, want at once (it must not wait for the child to read)", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Notify() did not return within 2s against a child that never reads its stdin")
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer flushCancel()
	flushStart := time.Now()
	err := conn.Flush(flushCtx)
	flushElapsed := time.Since(flushStart)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Flush() error = %v, want error wrapping context.DeadlineExceeded (the write is genuinely parked on the child's stdin)", err)
	}
	if flushElapsed > 2*time.Second {
		t.Errorf("Flush() took %v to time out, want close to its 300ms bound", flushElapsed)
	}

	closeErr := stdin.Close()
	if closeErr != nil {
		t.Logf("stdin.Close() error = %v (informational; the point is that WriteFailed closes afterward)", closeErr)
	}

	select {
	case <-conn.WriteFailed():
	case <-time.After(5 * time.Second):
		t.Fatal("WriteFailed() did not close within 5s after closing the child's stdin pipe")
	}

	conn.Close()
	releaseChild()

	waitForGoroutineCountAtOrBelow(t, baseline+1, 5*time.Second)
}
