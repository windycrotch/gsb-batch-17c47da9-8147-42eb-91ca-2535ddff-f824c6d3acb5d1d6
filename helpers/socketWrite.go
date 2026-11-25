package helpers

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Gorilla websocket connections permit at most one concurrent writer. During
// shutdown a socket can be written to simultaneously by its action goroutine,
// a room broadcast, and core.Pause() logout. writeLocks associates every
// upgraded connection with a mutex used to serialize all writes to it.
var writeLocks sync.Map // map[*websocket.Conn]*sync.Mutex

func writeLockFor(conn *websocket.Conn) *sync.Mutex {
	if m, ok := writeLocks.Load(conn); ok {
		return m.(*sync.Mutex)
	}
	candidate := &sync.Mutex{}
	actual, _ := writeLocks.LoadOrStore(conn, candidate)
	return actual.(*sync.Mutex)
}

// SafeWriteJSON writes JSON to conn while serializing against every other
// write to the same connection.
func SafeWriteJSON(conn *websocket.Conn, value interface{}) error {
	if conn == nil {
		return nil
	}
	mu := writeLockFor(conn)
	mu.Lock()
	defer mu.Unlock()
	return conn.WriteJSON(value)
}

// SafeWriteControl sends a control message while serializing against data
// writes to the same connection.
func SafeWriteControl(conn *websocket.Conn, messageType int, data []byte, deadline time.Time) error {
	if conn == nil {
		return nil
	}
	mu := writeLockFor(conn)
	mu.Lock()
	defer mu.Unlock()
	return conn.WriteControl(messageType, data, deadline)
}

// ReleaseWriteLock drops the per-connection write lock association after the
// connection has been closed. It is safe to call once when tearing a socket down.
func ReleaseWriteLock(conn *websocket.Conn) {
	if conn != nil {
		writeLocks.Delete(conn)
	}
}
