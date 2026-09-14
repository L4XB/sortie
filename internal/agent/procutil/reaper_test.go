package procutil

import (
	"bufio"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// TestReaper_DoneAndErr asserts that Done closes only once the
// subprocess has exited and that Err, read after that close, reports
// the same outcome cmd.Wait itself would have returned.
func TestReaper_DoneAndErr(t *testing.T) {
	t.Parallel()

	t.Run("non-zero exit", func(t *testing.T) {
		t.Parallel()

		cmd := fakeRuntimeCmd(t, agenttest.Output{ExitCode: 7})
		SetProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}

		r := StartReaper(cmd, nil)
		select {
		case <-r.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Done() did not close within 3s")
		}

		var exitErr *exec.ExitError
		if !errors.As(r.Err(), &exitErr) {
			t.Fatalf("Err() = %v (%T), want *exec.ExitError", r.Err(), r.Err())
		}
		if got := exitErr.ExitCode(); got != 7 {
			t.Errorf("Err().(*exec.ExitError).ExitCode() = %d, want 7", got)
		}
	})

	t.Run("clean exit", func(t *testing.T) {
		t.Parallel()

		cmd := fakeRuntimeCmd(t, agenttest.Output{})
		SetProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}

		r := StartReaper(cmd, nil)
		select {
		case <-r.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Done() did not close within 3s")
		}
		if err := r.Err(); err != nil {
			t.Errorf("Err() = %v, want nil", err)
		}
	})

	t.Run("Done stays open while the subprocess is still running", func(t *testing.T) {
		t.Parallel()

		cmd := fakeRuntimeCmd(t, agenttest.Output{Hang: true})
		SetProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}

		r := StartReaper(cmd, nil)
		select {
		case <-r.Done():
			t.Fatal("Done() closed before the subprocess exited")
		case <-time.After(200 * time.Millisecond):
		}

		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("cmd.Process.Kill() = %v", err)
		}
		select {
		case <-r.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Done() did not close after the subprocess was killed")
		}
	})
}

// TestReaper_OutputSurvivesAfterDoneCloses asserts that a child's
// standard-output line, written before it exits, is still readable
// once StartReaper's Done has closed: reaping the child does not close
// the caller-owned read end out from under a reader that has not yet
// consumed it.
func TestReaper_OutputSurvivesAfterDoneCloses(t *testing.T) {
	t.Parallel()

	cmd := fakeRuntimeCmd(t, agenttest.Output{Stdout: "hello from the child\n"})
	SetProcessGroup(cmd)

	pipes, err := StartWithOwnedPipes(cmd, nil)
	if err != nil {
		t.Fatalf("StartWithOwnedPipes() error = %v", err)
	}
	t.Cleanup(func() { pipes.Close() }) //nolint:errcheck // best-effort cleanup

	r := StartReaper(cmd, nil)
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close within 5s")
	}

	line, err := bufio.NewReader(pipes.Stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read standard output after Done closed: %v", err)
	}
	if want := "hello from the child\n"; line != want {
		t.Errorf("standard output = %q, want %q", line, want)
	}
}

// TestReaper_ClosingStdoutAtDoneLosesOutput is the negative control for
// TestReaper_OutputSurvivesAfterDoneCloses: closing the read end at the
// instant Done closes, the way an unowned pipe closes automatically at
// that point, loses the line the positive case recovers. A green
// result here would mean the positive case never actually raced the
// reap, and the fixture, not production code, would be what to fix.
func TestReaper_ClosingStdoutAtDoneLosesOutput(t *testing.T) {
	t.Parallel()

	cmd := fakeRuntimeCmd(t, agenttest.Output{Stdout: "hello from the child\n"})
	SetProcessGroup(cmd)

	pipes, err := StartWithOwnedPipes(cmd, nil)
	if err != nil {
		t.Fatalf("StartWithOwnedPipes() error = %v", err)
	}
	t.Cleanup(func() { pipes.Close() }) //nolint:errcheck // best-effort cleanup

	r := StartReaper(cmd, nil)
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close within 5s")
	}
	if err := pipes.CloseStdout(); err != nil {
		t.Fatalf("CloseStdout() error = %v", err)
	}

	if line, err := bufio.NewReader(pipes.Stdout).ReadString('\n'); err == nil {
		t.Fatalf("read standard output after closing it at the reap = %q, want an error (the negative control did not reproduce the loss)", line)
	}
}
