package noisesuppressionstream

import "sync"

// boundedBuffer is a FIFO byte buffer with a fixed byte capacity, guarded by
// an externally supplied mutex and coordinated through condition variables.
//
// Unlike a ring buffer it stores data in a single contiguous region, so a
// message can never be split across a wrap point and come back reordered.
// Consumed head bytes are reclaimed by compaction when the backing array
// fills up; growth is bounded by cap, never exceeding it.
//
// All methods must be called while holding mu. It is intended for a single
// writer and one or more readers serialized externally.
type boundedBuffer struct {
	mu    *sync.Mutex
	data  []byte
	start int
	cap   int
}

func newBoundedBuffer(mu *sync.Mutex, capacity int) *boundedBuffer {
	return &boundedBuffer{
		mu:   mu,
		data: make([]byte, 0, capacity),
		cap:  capacity,
	}
}

// Len returns the number of unread bytes.
func (b *boundedBuffer) Len() int {
	return len(b.data) - b.start
}

// Space returns the capacity still available for writes.
func (b *boundedBuffer) Space() int {
	return b.cap - b.Len()
}

// compact moves the unread prefix to the front of the backing array,
// reclaiming space consumed from the head.
func (b *boundedBuffer) compact() {
	if b.start == 0 {
		return
	}
	unread := b.Len()
	copy(b.data[:unread], b.data[b.start:])
	b.data = b.data[:unread]
	b.start = 0
}

// Write appends as many bytes of p as currently fit and returns the number
// written (which may be zero when the buffer is full). The caller applies
// backpressure and retries the remainder.
func (b *boundedBuffer) Write(p []byte) int {
	if b.Space() == 0 || len(p) == 0 {
		return 0
	}
	b.compact()
	room := b.cap - len(b.data)
	n := len(p)
	if n > room {
		n = room
	}
	b.data = append(b.data, p[:n]...)
	return n
}

// Read copies up to len(p) unread bytes into p and returns the number copied,
// which may be zero when no data is available.
func (b *boundedBuffer) Read(p []byte) int {
	if b.Len() == 0 || len(p) == 0 {
		return 0
	}
	n := copy(p, b.data[b.start:])
	b.start += n
	if b.start == len(b.data) {
		// Everything buffered was consumed: reset to keep the backing array.
		b.data = b.data[:0]
		b.start = 0
	}
	return n
}
