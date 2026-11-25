// Copyright 2018 The ZikiChombo Authors. All rights reserved.  Use of this source
// code is governed by a license that can be found in the License file.

package ops

import (
	"errors"
	"io"
	"sync"

	"github.com/zikichombo/sound"
)

// errLoopNoProgress is returned if the wrapped source returns no frames
// without reporting io.EOF, which would otherwise make the loop spin
// forever.
var errLoopNoProgress = errors.New("loop: underlying source made no progress and returned no error")

type loop struct {
	sound.SourceSeeker

	// mu serializes Receive calls and protects the bookkeeping fields
	// (n, atEnd, buf).  Close never takes mu: it signals closure through
	// closeCh and then closes the underlying source, which unblocks a
	// Receive waiting on that source, so closing an infinite loop can
	// always make progress.
	mu sync.Mutex

	// n is the number of plays remaining.  A negative value means the
	// loop repeats forever.
	n int

	// atEnd is true once the underlying source has reported io.EOF and
	// before the loop rewinds it.  It distinguishes "the final frames of
	// a play arrived together with EOF" from "a read started at the end".
	atEnd bool

	// closeOnce makes Close idempotent; closeCh is closed exactly once
	// to signal closure to any in-flight Receive.
	closeOnce sync.Once
	closeCh   chan struct{}

	// scratch holds one Receive() worth of deinterleaved frames read
	// from the underlying source before they are copied into dst.
	buf []float64
}

// Receive implements sound.Source.
//
// The data is deinterleaved and every returned frame contains one sample
// per channel from the same point in time.  When the underlying source
// reaches its end, the loop seeks back to the start without dropping or
// duplicating a frame.  A finite loop returns (0, io.EOF) once all plays
// have been delivered; an infinite loop keeps going until Close.
func (l *loop) Receive(dst []float64) (int, error) {
	nC := l.Channels()
	if len(dst)%nC != 0 {
		return 0, sound.ErrChannelAlignment
	}
	if l.isClosed() {
		return 0, io.EOF
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.isClosed() {
		return 0, io.EOF
	}
	// n == 0 means every finite play has already been delivered.
	if l.n == 0 {
		return 0, io.EOF
	}
	nF := len(dst) / nC
	if nF == 0 {
		return 0, nil
	}

	if cap(l.buf) < len(dst) {
		l.buf = make([]float64, len(dst))
	}
	tmp := l.buf[:len(dst)]

	f := 0 // frames placed in dst so far
	for f < nF {
		// Re-check closure on every iteration; Close may have fired
		// while we were inside the underlying source.
		if l.isClosed() {
			return f, io.EOF
		}

		// A previous read left the source positioned at its end.  This
		// zero-frame EOF is the boundary between plays: the finished
		// play was already counted, so just rewind (or stop).
		if l.atEnd {
			if l.n == 0 {
				return f, io.EOF
			}
			if e := l.Seek(0); e != nil {
				// Rewind failed: do not keep reading from the stale,
				// exhausted position.
				return 0, e
			}
			l.atEnd = false
		}

		n, err := l.SourceSeeker.Receive(tmp[:(nF-f)*nC])
		if err != nil && err != io.EOF {
			// Surface real errors instead of swallowing them.
			return 0, err
		}
		if n < 0 {
			return 0, errors.New("loop: underlying source returned a negative frame count")
		}

		if n > 0 {
			// Copy the n deinterleaved frames into the correct
			// per-channel positions of dst.  This never overwrites
			// frames already delivered.
			for c := 0; c < nC; c++ {
				copy(dst[c*nF+f:c*nF+f+n], tmp[c*n:(c+1)*n])
			}
			f += n
		}

		if err == io.EOF {
			// One play is now fully delivered.  Count it exactly once
			// and remember the source is parked at its end.
			l.atEnd = true
			if l.n > 0 {
				l.n--
			}
			if l.n == 0 {
				// Finite loop exhausted.  Frames read this call (if any)
				// are returned now; the next call yields (0, io.EOF).
				if f == 0 {
					return 0, io.EOF
				}
				return f, nil
			}
			// More plays (or infinite): rewind at the top of the next
			// iteration, after the closed-state re-check.
			continue
		}

		if n == 0 {
			// No EOF and no frames: reading again would spin forever.
			return 0, errLoopNoProgress
		}
	}
	return f, nil
}

// isClosed reports whether Close has been called.  It is safe to call
// without holding mu.
func (l *loop) isClosed() bool {
	select {
	case <-l.closeCh:
		return true
	default:
		return false
	}
}

// Close signals closure and then closes the underlying source.  Infinite
// loops stop as soon as possible: an in-flight Receive is interrupted as
// soon as the underlying source honors its own Close, and every later
// Receive returns io.EOF.  Close is idempotent.
func (l *loop) Close() error {
	first := false
	l.closeOnce.Do(func() {
		first = true
		close(l.closeCh)
	})
	if !first {
		return nil
	}
	return l.SourceSeeker.Close()
}

// Loop creates a src which is src looped n times.
// Loop will loop infinitely if n < 0 and will
// terminate with EOF immediately if n == 0.
func Loop(src sound.SourceSeeker, n int) sound.Source {
	return &loop{
		SourceSeeker: src,
		n:            n,
		closeCh:      make(chan struct{}),
	}
}
