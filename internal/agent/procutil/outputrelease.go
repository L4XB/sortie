package procutil

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// OutputAbandonedMessage is a turn's terminal message once the release
// gave up on the connection's reader. It states what the adapter
// observed, and names no channel, field, method, or file.
const OutputAbandonedMessage = "the agent runtime exited before the session finished collecting its output"

// OutputAbandonedWarning is the record the give-up arm logs. It says
// "session", not "turn", because every kind that uses it holds one
// subprocess across every turn.
const OutputAbandonedWarning = "agent stdout was not fully collected before the session ended"

// OutputReleaseParams configures [StartOutputRelease]. Every channel is
// taken as a channel rather than as the object that owns it, so a test
// drives the sequence from hand-made channels with no subprocess.
type OutputReleaseParams struct {
	// Pipes holds the read ends the release closes on the give-up arm.
	// Required.
	Pipes *OwnedPipes

	// Reaped closes once the subprocess has been reaped. Required.
	Reaped <-chan struct{}

	// ReaderDone closes once the consumer's own reader has ended.
	// Required.
	ReaderDone <-chan struct{}

	// Stderr, when non-nil, is drained and released on its own
	// goroutine, anchored on the same reap and bounded by the same
	// Grace.
	Stderr *StderrCollector

	// Grace bounds the post-reap wait for ReaderDone. A non-positive
	// value resolves to DefaultDrainGrace.
	Grace time.Duration

	// OnAbandon, when non-nil, runs after the standard-output read end
	// is closed on the give-up arm.
	OnAbandon func()

	// Logger receives the give-up arm's warning. A nil Logger resolves
	// to slog.Default.
	Logger *slog.Logger
}

// OutputRelease is one session's post-reap release of the reader parked
// on a dead runtime's standard output.
type OutputRelease struct {
	abandoned chan struct{}
	latch     atomic.Bool
}

// StartOutputRelease starts one goroutine that waits for the subprocess
// to be reaped, then gives the connection's own reader up to
// params.Grace to end before giving up on it. It starts a second
// goroutine when params.Stderr is non-nil. It panics when params.Pipes,
// params.Reaped, or params.ReaderDone is nil.
func StartOutputRelease(params OutputReleaseParams) *OutputRelease {
	if params.Pipes == nil {
		panic("procutil: StartOutputRelease requires a non-nil Pipes")
	}
	if params.Reaped == nil {
		panic("procutil: StartOutputRelease requires a non-nil Reaped")
	}
	if params.ReaderDone == nil {
		panic("procutil: StartOutputRelease requires a non-nil ReaderDone")
	}

	grace := params.Grace
	if grace <= 0 {
		grace = DefaultDrainGrace
	}
	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	r := &OutputRelease{abandoned: make(chan struct{})}

	go func() {
		<-params.Reaped

		// Closing the read end once the bound has resolved is what ends
		// the drain itself: abandoning it releases whoever waited for the
		// lines, while the scanner stays blocked in a read for as long as
		// an escaped descendant holds the write end.
		if params.Stderr != nil {
			go func() {
				params.Stderr.FinishAndCollect(grace)
				params.Pipes.CloseStderr() //nolint:errcheck,gosec // best-effort; ends a drain nothing else can
			}()
		}

		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-params.ReaderDone:
		case <-timer.C:
			// The latch is stored before the abandonment channel closes
			// and before the read end closes, so every consumer released
			// by either signal observes it already set.
			r.latch.Store(true)
			close(r.abandoned)
			params.Pipes.CloseStdout() //nolint:errcheck,gosec // best-effort; releases a reader parked on a dead runtime's descendant
			if params.OnAbandon != nil {
				params.OnAbandon()
			}
			logger.Warn(OutputAbandonedWarning, slog.Duration("drain_bound", grace))
		}
	}()

	return r
}

// Abandoned returns a channel closed once the release gave up. A nil
// receiver returns nil, which blocks a select arm forever.
func (r *OutputRelease) Abandoned() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.abandoned
}

// TurnEndMessage returns [OutputAbandonedMessage] once the release gave
// up, and fallback otherwise. A nil receiver returns fallback.
func (r *OutputRelease) TurnEndMessage(fallback string) string {
	if r == nil {
		return fallback
	}
	if r.latch.Load() {
		return OutputAbandonedMessage
	}
	return fallback
}
