//go:build unix

package procutil

import (
	"log/slog"
	"os/exec"
	"time"
)

// startAndAssign places cmd in its own process group and starts it.
// Job Object assignment has no Unix analogue: process-group membership
// is established at fork time via Setpgid, so keepJobHandle is unused
// and the returned handle is always zero. The returned time is the
// moment cmd.Start returned, the zero value when it failed.
func startAndAssign(cmd *exec.Cmd, _ *slog.Logger, _ bool) (uintptr, time.Time, error) {
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return 0, time.Time{}, err
	}
	startedAt := time.Now()
	_ = assignProcess(cmd.Process.Pid, cmd.Process)
	return 0, startedAt, nil
}

// drainCaptureJob is a no-op on Unix: there is no Job Object to drain.
func drainCaptureJob(_ uintptr, _ *exec.Cmd, _ time.Time, _ int64, _ *slog.Logger) {}
