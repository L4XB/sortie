//go:build unix

package procutil

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func init() {
	fakeScenarios["procutil.group-leader"] = agenttest.Typed(runGroupLeader)
	fakeScenarios["procutil.group-descendant"] = agenttest.Typed(runGroupDescendant)
}

func TestSetProcessGroup(t *testing.T) {
	t.Parallel()

	t.Run("nil SysProcAttr", func(t *testing.T) {
		t.Parallel()

		cmd := &exec.Cmd{}
		SetProcessGroup(cmd)

		if cmd.SysProcAttr == nil {
			t.Fatal("SetProcessGroup() SysProcAttr = nil, want non-nil")
		}
		if !cmd.SysProcAttr.Setpgid {
			t.Error("SetProcessGroup() Setpgid = false, want true")
		}
	})

	t.Run("existing SysProcAttr fields preserved", func(t *testing.T) {
		t.Parallel()

		cmd := &exec.Cmd{
			SysProcAttr: &syscall.SysProcAttr{Noctty: true},
		}
		SetProcessGroup(cmd)

		if !cmd.SysProcAttr.Setpgid {
			t.Error("SetProcessGroup() Setpgid = false, want true")
		}
		if !cmd.SysProcAttr.Noctty {
			t.Error("SetProcessGroup() Noctty = false, want true (pre-existing field must be preserved)")
		}
	})
}

func TestSignalProcessGroup_ESRCH(t *testing.T) {
	t.Parallel()

	// math.MaxInt32 is an implausible PID; no such process group can exist.
	err := SignalProcessGroup(math.MaxInt32, syscall.SIGTERM)
	if err != nil {
		t.Errorf("SignalProcessGroup(MaxInt32, SIGTERM) = %v, want nil (ESRCH must be suppressed)", err)
	}
}

func TestSignalGraceful_ESRCH(t *testing.T) {
	t.Parallel()

	// math.MaxInt32 is an implausible PID; ESRCH is silently swallowed.
	err := SignalGraceful(math.MaxInt32)
	if err != nil {
		t.Errorf("SignalGraceful(MaxInt32) = %v, want nil (ESRCH must be suppressed)", err)
	}
}

func TestSignalProcessGroup_LiveProcess(t *testing.T) {
	t.Parallel()

	cmd := fakeRuntimeCmd(t, agenttest.Output{Hang: true})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}

	if err := SignalProcessGroup(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("SignalProcessGroup(pid, SIGTERM) = %v, want nil", err)
	}

	err := cmd.Wait()
	if !WasSignaled(err) {
		t.Errorf("WasSignaled(cmd.Wait()) = false, want true (process should have been terminated by SIGTERM)")
	}
}

// pollForPID polls path until it holds a positive integer, returning it.
func pollForPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pollForPID(%q) = no PID after %v, want a PID", path, timeout)
	return 0
}

// pollForFile reports whether path appears before the timeout expires.
func pollForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// groupLeaderParams parameterizes the procutil.group-leader scenario: a
// fake runtime that starts a descendant fake runtime and waits for it,
// remaining a member of the process group SetGroupCancel places its own
// launch command into.
type groupLeaderParams struct {
	DescendantPath string
}

func runGroupLeader(_ []string, params groupLeaderParams) int {
	// Disables the default, uncatchable SIGTERM disposition so the
	// leader survives long enough to wait for (and so reap) the
	// descendant, rather than leaving it a zombie for the test's
	// kill(pid, 0) liveness check to trip over.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM)

	cmd := exec.Command(params.DescendantPath) //nolint:gosec // fake runtime path under t.TempDir()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "group leader: start descendant: %v\n", err)
		return 1
	}
	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "group leader: wait for descendant: %v\n", err)
		return 1
	}
	return 0
}

// groupDescendantParams parameterizes the procutil.group-descendant
// scenario: a fake runtime that records its own PID, then traps a
// catchable termination signal and records that it caught one.
type groupDescendantParams struct {
	Marker  string
	PIDFile string
}

func runGroupDescendant(_ []string, params groupDescendantParams) int {
	// The handler is installed before the PID file publishes readiness:
	// the test cancels as soon as it reads that file, and the default
	// disposition would end this process before it records the signal.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)

	if err := os.WriteFile(params.PIDFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "group descendant: write pid: %v\n", err)
		return 1
	}

	<-sig

	if err := os.WriteFile(params.Marker, []byte("terminated"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "group descendant: write marker: %v\n", err)
		return 1
	}
	return 0
}

// TestSetGroupCancel_CancelReachesDescendant verifies that cancelling the
// context of a command prepared by SetGroupCancel delivers a catchable
// termination signal to the whole process group, not just to the direct
// child.
//
// The evidence is a marker a grandchild writes from inside its own
// signal handler. A grandchild is reachable only through the group, and
// it can only run a handler if the signal was catchable, so the marker
// distinguishes a group-wide graceful signal from os/exec's default of
// force-killing the direct child alone. The leader waits for the
// descendant before exiting, so it is reaped rather than left a zombie,
// and cmd.Wait cannot return until the marker is on disk.
func TestSetGroupCancel_CancelReachesDescendant(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "descendant.terminated")
	pidFile := filepath.Join(dir, "descendant.pid")

	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", "procutil.group-descendant", groupDescendantParams{
		Marker:  marker,
		PIDFile: pidFile,
	})
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.group-leader", groupLeaderParams{
		DescendantPath: descendantPath,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	SetGroupCancel(cmd, DefaultStopGrace)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v, want nil", err)
	}
	leaderPID := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-leaderPID, syscall.SIGKILL) })

	descendantPID := pollForPID(t, pidFile, 5*time.Second)

	cancel()
	_ = cmd.Wait() //nolint:errcheck // a cancelled command reports the cancellation, not a fault

	if !pollForFile(marker, 5*time.Second) {
		t.Errorf("SetGroupCancel(): cancelling left %q absent, want the descendant to have caught a graceful signal", marker)
	}
	if err := syscall.Kill(descendantPID, 0); err == nil {
		t.Errorf("SetGroupCancel(): descendant %d still alive after cancellation, want gone", descendantPID)
	}
}
