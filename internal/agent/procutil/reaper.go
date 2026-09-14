package procutil

import "os/exec"

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
func StartReaper(cmd *exec.Cmd) *Reaper {
	r := &Reaper{done: make(chan struct{})}
	go func() {
		r.err = cmd.Wait()
		r.leftover, r.cleanupErr = killProcessGroupReportingLeftover(cmd.Process.Pid)
		CleanupProcess(cmd.Process.Pid)
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

// CleanupErr returns the error the group termination reported, or nil
// when it succeeded or found the group already gone. Call only after
// Done closed.
//
// A non-nil value means the reap could not prove the process tree was
// torn down, so a descendant may have survived it; Leftover reports
// nothing about such a descendant, because the membership check runs
// before the termination that failed.
func (r *Reaper) CleanupErr() error {
	return r.cleanupErr
}
