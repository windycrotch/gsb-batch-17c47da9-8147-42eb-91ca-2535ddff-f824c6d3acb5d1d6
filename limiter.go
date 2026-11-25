package main

import (
	"context"
	"sync"
)

// DefaultMaxSessions is the default upper bound on concurrent streaming
// sessions. It is documented in the README configuration table.
const DefaultMaxSessions = 2

// BusyMessage is the response body returned when a new session is rejected
// because the server is already at capacity.
const BusyMessage = "Server busy: maximum concurrent stream sessions reached. Please retry later."

// SessionManager bounds the number of concurrent streaming sessions and keeps
// track of their cancellation functions so that a graceful shutdown can
// terminate in-flight streams.
//
// A session adopts a slot before any subprocess is started and gives the slot
// back as soon as the session ends: normal completion, client disconnect or an
// upstream failure. Acquire never blocks, so callers can reject excess requests
// immediately, before launching yt-dlp or ffmpeg.
type SessionManager struct {
	slots chan struct{}

	mu       sync.Mutex
	nextID   uint64
	active   map[uint64]context.CancelFunc
	shutdown bool
}

// NewSessionManager creates a manager that admits at most max concurrent
// sessions. Values below 1 are clamped to 1.
func NewSessionManager(max int) *SessionManager {
	if max < 1 {
		max = 1
	}
	return &SessionManager{
		slots:  make(chan struct{}, max),
		active: make(map[uint64]context.CancelFunc),
	}
}

// Acquire reserves a slot without blocking.
//
// The supplied cancel function is associated with the session so that
// Shutdown can abort it. On success the returned release function frees the
// slot exactly once and unregisters the session; calling it more than once is
// safe and never frees additional capacity. On failure it returns (nil, false);
// no slot is held and cancel is not registered.
func (m *SessionManager) Acquire(cancel context.CancelFunc) (release func(), ok bool) {
	// A nil manager means "no limit"; callers need not special-case it.
	if m == nil {
		return func() {}, true
	}

	select {
	case m.slots <- struct{}{}:
	default:
		return nil, false
	}

	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		<-m.slots
		return nil, false
	}
	m.nextID++
	id := m.nextID
	m.active[id] = cancel
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			// Drain the token before unregistering the session so that an
			// observed Active()==0 always implies the capacity is available
			// again. The receive cannot block: this session holds exactly one
			// token, and only this once-guarded release consumes it.
			<-m.slots
			delete(m.active, id)
			m.mu.Unlock()
		})
	}, true
}

// Shutdown prevents new sessions from being admitted and cancels every active
// session so that their subprocesses terminate and their slots are released.
// It is safe to call more than once.
func (m *SessionManager) Shutdown() {
	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		return
	}
	m.shutdown = true
	cancels := make([]context.CancelFunc, 0, len(m.active))
	for _, cancel := range m.active {
		cancels = append(cancels, cancel)
	}
	m.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

// Active returns the number of sessions currently holding a slot.
func (m *SessionManager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// Capacity returns the configured maximum number of concurrent sessions.
func (m *SessionManager) Capacity() int {
	if m == nil {
		return 0
	}
	return cap(m.slots)
}
