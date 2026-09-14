package procutil

import (
	"log/slog"
	"os/exec"
	"path/filepath"
	"time"
)

// groupDrainBound bounds the wait, after a process group or Job Object
// termination, for it to report no member remaining. Only a test
// replaces it, to shorten the wait for a group that never settles.
var groupDrainBound = 2 * time.Second

// groupDrainPollInterval is the pause between successive membership
// polls while waiting for a terminated process group or Job Object to
// settle.
const groupDrainPollInterval = 5 * time.Millisecond

// Reaper reaps one started subprocess and terminates its process group.
type Reaper struct {
	done       chan struct{}
	err        error
	leftover   bool
	cleanupErr error
}

// StartReaper starts one goroutine that waits for cmd to exit, kills its
// process group, releases the platform's process-group resources, and
// only then closes the channel Done returns. cmd MUST already be
// started.
//
// A termination that could not prove the process tree torn down is
// logged as the one [CaptureCleanupWarning] record for this reap; every
// caller sees that record regardless of which one of them later reads
// [Reaper.CleanupErr]. If logger is nil, StartReaper uses
// [slog.Default].
func StartReaper(cmd *exec.Cmd, logger *slog.Logger) *Reaper {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Reaper{done: make(chan struct{})}
	go func() {
		r.err = cmd.Wait()
		r.leftover, r.cleanupErr = killProcessGroupReportingLeftover(cmd.Process.Pid)
		CleanupProcess(cmd.Process.Pid)

		// A termination that failed leaves the process tree unproven, so
		// this record is the only thing standing between a surviving
		// descendant and a launch that looks cleanly torn down.
		if r.cleanupErr != nil {
			logger.Warn(CaptureCleanupWarning,
				slog.String("command", filepath.Base(cmd.Path)),
				slog.Any("error", r.cleanupErr))
		}

		close(r.done)
	}()
	return r
}

// Done closes once the subprocess has been reaped, its process group
// killed, and its platform resources released.
func (r *Reaper) Done() <-chan struct{} {
	return r.done
}

// Err returns the error cmd.Wait reported. Call only after Done closed.
func (r *Reaper) Err() error {
	return r.err
}

// Leftover reports whether the group termination reached a process of
// cmd's process group or Job Object other than the direct child. Call
// only after Done closed.
func (r *Reaper) Leftover() bool {
	return r.leftover
}

// CleanupErr returns the error the group termination reported,
// including a bounded wait for the group to report itself empty
// timing out, or nil when it succeeded within that wait or found the
// group already gone. Call only after Done closed.
//
// A non-nil value means the reap could not prove the process tree was
// torn down, so a descendant may have survived it; Leftover reports
// nothing about such a descendant, because its membership check runs
// before this outcome is known.
func (r *Reaper) CleanupErr() error {
	return r.cleanupErr
}
