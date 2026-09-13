package procutil

import "io"

// CloseWithoutWaiting closes closer on its own goroutine and returns
// at once, regardless of how long Close takes to return or whether it
// ever does. closer may be nil, in which case this does nothing.
//
// On Windows, (*os.File).Close on a pipe waits for a pending write to
// be cancelled through CancelIoEx, and that cancellation is not
// guaranteed to complete. A caller closing standard input during
// teardown needs the signal, the wait, and the kill that follow it to
// run on schedule regardless, so it must not block on this close
// synchronously on any platform.
func CloseWithoutWaiting(closer io.Closer) {
	if closer == nil {
		return
	}
	go func() {
		_ = closer.Close()
	}()
}
