package jsonrpc

import "sync"

// Inbox is an unbounded, ordered, lossless queue of delivered values.
// Put and the Sink Deliver builds never wait, however much the queue
// already holds; Take is the only way values leave it. Construct one
// with NewInbox.
//
// Put, Close, and Take are all safe for concurrent use from any
// goroutine, but only one goroutine may call Take at a time: a second
// concurrent Take could otherwise observe the same wake on Ready and
// find nothing queued.
type Inbox[T any] struct {
	mu     sync.Mutex
	items  []T           // guarded by mu; oldest first
	closed bool          // guarded by mu
	ready  chan struct{} // capacity 1; invariant: holds a value iff items is non-empty or closed
}

// inboxSink adapts an Inbox into a Sink by wrapping every delivered
// Message before appending it.
type inboxSink[T any] struct {
	inbox *Inbox[T]
	wrap  func(Message) T
}

// NewInbox returns an open, empty inbox.
func NewInbox[T any]() *Inbox[T] {
	return &Inbox[T]{ready: make(chan struct{}, 1)}
}

// Put appends v behind every item already queued, returns without
// waiting, and reports true. After [Inbox.Close] it discards v and
// reports false.
func (b *Inbox[T]) Put(v T) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.items = append(b.items, v)
	b.settle()
	return true
}

// Close marks the end of input. It is idempotent. Items queued before
// Close stay takeable; a value Put after Close is never taken.
func (b *Inbox[T]) Close() {
	b.mu.Lock()
	b.closed = true
	b.settle()
	b.mu.Unlock()
}

// Ready returns the same channel on every call. The channel holds a
// value exactly when an item is queued or the inbox is closed, so a
// closed inbox stays receivable once every queued item has been taken.
func (b *Inbox[T]) Ready() <-chan struct{} {
	return b.ready
}

// Take removes and returns the oldest queued item. ok is false when
// nothing is queued: after a receive from [Inbox.Ready], that means
// the inbox is closed and empty. Take releases every reference to the
// item it removes and releases any storage the queue does not need to
// keep.
func (b *Inbox[T]) Take() (v T, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.items) == 0 {
		b.settle()
		return v, false
	}

	item := b.items[0]
	var zero T
	b.items[0] = zero
	b.items = b.items[1:]
	if len(b.items) == 0 {
		b.items = nil
	}
	b.settle()
	return item, true
}

// settle keeps ready holding a value exactly when items is non-empty
// or closed. Callers must hold mu.
func (b *Inbox[T]) settle() {
	if len(b.items) > 0 || b.closed {
		select {
		case b.ready <- struct{}{}:
		default:
		}
		return
	}
	select {
	case <-b.ready:
	default:
	}
}

// deliver appends wrap(msg) to inbox.
func (s *inboxSink[T]) deliver(msg Message) {
	s.inbox.Put(s.wrap(msg))
}

// Sink is the only delivery target [NewConn] accepts. Its method is
// unexported, which closes its implementation set to this package the
// way [testing.TB] closes its own: build one with [Deliver].
type Sink interface {
	deliver(msg Message)
}

// Deliver returns the [Sink] that appends wrap(msg) to inbox, for
// every message [NewConn] delivers.
//
// Deliver panics when inbox or wrap is nil. wrap runs on the
// connection's reader goroutine and MUST return without blocking and
// without calling the connection, or every read behind it stalls.
func Deliver[T any](inbox *Inbox[T], wrap func(Message) T) Sink {
	if inbox == nil {
		panic("jsonrpc: inbox must be non-nil")
	}
	if wrap == nil {
		panic("jsonrpc: wrap must be non-nil")
	}
	return &inboxSink[T]{inbox: inbox, wrap: wrap}
}
