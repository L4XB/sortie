//go:build unix

package procutil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// SetProcessGroup configures cmd to start in its own process group.
// Must be called before [exec.Cmd.Start]. Any pre-existing
// [syscall.SysProcAttr] fields are preserved.
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// SignalProcessGroup sends sig to the entire process group led by pid.
// Returns nil if the process group no longer exists (ESRCH), since
// group expiry is expected during best-effort cleanup.
func SignalProcessGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// KillProcessGroup sends SIGKILL to the entire process group led by
// pid. Returns nil if the process group no longer exists.
func KillProcessGroup(pid int) error {
	_, err := killProcessGroupReportingLeftover(pid)
	return err
}

// killProcessGroupReportingLeftover sends SIGKILL to the process group
// led by pid and reports whether the group still answered: a direct
// child StartReaper has already reaped no longer belongs to the group,
// so syscall.Kill succeeding means at least one other member was still
// alive to receive the signal, read before SignalProcessGroup maps
// ESRCH to nil.
func killProcessGroupReportingLeftover(pid int) (leftover bool, err error) {
	killErr := syscall.Kill(-pid, syscall.SIGKILL)
	if killErr == nil {
		return true, nil
	}
	if errors.Is(killErr, syscall.ESRCH) {
		return false, nil
	}
	return false, killErr
}

// SignalGraceful sends SIGTERM to the entire process group led by pid.
func SignalGraceful(pid int) error {
	return SignalProcessGroup(pid, syscall.SIGTERM)
}

// assignProcess is a no-op on Unix. Process group membership is
// established at fork time via Setpgid.
func assignProcess(_ int, _ *os.Process) error { return nil }

// CleanupProcess is a no-op on Unix. Process group resources are
// managed by the kernel.
func CleanupProcess(_ int) {}
