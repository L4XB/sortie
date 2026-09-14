package procutil

import "os/exec"

// Reaper reaps one started subprocess and terminates its process group.
type Reaper struct {
	done     chan struct{}
	err      error
	leftover bool
}

// StartReaper starts one goroutine that waits for cmd to exit, kills its
// process group, releases the platform's process-group resources, and
// only then closes the channel Done returns. cmd MUST already be
// started.
func StartReaper(cmd *exec.Cmd) *Reaper {
	r := &Reaper{done: make(chan struct{})}
	go func() {
		r.err = cmd.Wait()
		r.leftover, _ = killProcessGroupReportingLeftover(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup of surviving group members
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
