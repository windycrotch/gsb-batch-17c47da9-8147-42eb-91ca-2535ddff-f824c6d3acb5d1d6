// Copyright 2018 The ZikiChombo Authors. All rights reserved.  Use of this source
// code is governed by a license that can be found in the License file.

package ops

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/zikichombo/sound"
	"github.com/zikichombo/sound/freq"
)

// multiError aggregates the errors returned by closing more than one
// underlying source.  It is used instead of errors.Join so that the
// package keeps building on Go versions older than 1.20.
type multiError struct {
	errs []error
}

func (m *multiError) Error() string {
	parts := make([]string, len(m.errs))
	for i, e := range m.errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

type join struct {
	// mu serializes Receive calls and protects the bookkeeping fields
	// (done, buf).  Close never takes mu: it signals closure through
	// closeCh and then closes the underlying sources, which unblocks any
	// Receive waiting on those sources.
	mu sync.Mutex

	srcs []sound.Source

	// done[i] is set once source i has signaled io.EOF.  An exhausted
	// source is never called again; its channel block is zero filled.
	done []bool

	// scratch used when a source carries more than one channel, so
	// that its deinterleaved channels can be scattered into the
	// correct output channel positions.
	buf []float64

	// closeOnce makes Close idempotent; closeCh is closed exactly once
	// to signal closure to any in-flight Receive.
	closeOnce sync.Once
	closeCh   chan struct{}
}

func (j *join) SampleRate() freq.T {
	return j.srcs[0].SampleRate()
}

// Channels returns the total number of output channels, namely the
// sum of the channel counts of the joined sources.  All sources are
// guaranteed to share the same channel count by Compat.
func (j *join) Channels() int {
	nC := 0
	for _, s := range j.srcs {
		nC += s.Channels()
	}
	return nC
}

// Receive implements sound.Source.
//
// The returned data is deinterleaved: every frame returned contains one
// sample for every output channel, and all samples of a given frame
// originate from the same point in time.  A source which ends before the
// others contributes zeros for every frame position past its end, and
// data already placed in dst is never overwritten by later copies.
//
// Receive returns the largest frame count any source could provide in
// this call.  Once every source is exhausted, Receive returns (0, io.EOF).
func (j *join) Receive(dst []float64) (int, error) {
	nC := j.Channels()
	if len(dst)%nC != 0 {
		return 0, sound.ErrChannelAlignment
	}
	if j.isClosed() {
		return 0, io.EOF
	}
	if len(dst) == 0 {
		return 0, nil
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	nF := len(dst) / nC // requested frame count
	nf := 0             // maximum frame count actually available
	base := 0           // output channel offset of the current source

	for si, src := range j.srcs {
		// A close may have happened while a previous source call was
		// blocked; stop producing data promptly.
		if j.isClosed() {
			return 0, io.EOF
		}

		cS := src.Channels()

		if j.done[si] {
			// Exhausted source: zero fill every channel it owns.
			for k := 0; k < cS; k++ {
				out := base + k
				blk := dst[out*nF : (out+1)*nF]
				for f := range blk {
					blk[f] = 0
				}
			}
			base += cS
			continue
		}

		var m int
		var err error
		if cS == 1 {
			// Mono sources own a single contiguous output block, so
			// they can write directly into dst without a scratch buffer.
			region := dst[base*nF : (base+1)*nF]
			m, err = src.Receive(region)
		} else {
			if cap(j.buf) < cS*nF {
				j.buf = make([]float64, cS*nF)
			}
			tmp := j.buf[:cS*nF]
			m, err = src.Receive(tmp)
			if err == nil || err == io.EOF {
				for k := 0; k < cS; k++ {
					out := base + k
					// Scatter the valid frames into the matching output
					// channel block.
					copy(dst[out*nF:out*nF+m], tmp[k*m:(k+1)*m])
				}
			}
		}

		if j.isClosed() {
			// Close interrupted an underlying (possibly blocked) read.
			return 0, io.EOF
		}
		if err != nil && err != io.EOF {
			return 0, err
		}
		if err == io.EOF {
			j.done[si] = true
			m = 0
		}
		if m < 0 || m > nF {
			return 0, fmt.Errorf("join: source %d returned invalid frame count %d", si, m)
		}

		// Zero fill the tail of every channel this source owns.  The
		// valid frames in the prefix are never touched again.
		for k := 0; k < cS; k++ {
			out := base + k
			for f := m; f < nF; f++ {
				dst[out*nF+f] = 0
			}
		}

		if m > nf {
			nf = m
		}
		base += cS
	}

	if nf == 0 {
		// Nothing available from any source: the join is exhausted.
		return 0, io.EOF
	}
	return nf, nil
}

// isClosed reports whether Close has been called.  It is safe to call
// without holding mu.
func (j *join) isClosed() bool {
	select {
	case <-j.closeCh:
		return true
	default:
		return false
	}
}

// Close closes every underlying source, even if one of them fails, and
// aggregates every non-nil error.  After Close, Receive always returns
// (0, io.EOF).  Close is idempotent and does not block on an in-flight
// Receive.
func (j *join) Close() error {
	first := false
	j.closeOnce.Do(func() {
		first = true
		close(j.closeCh)
	})
	if !first {
		return nil
	}

	var errs []error
	for _, src := range j.srcs {
		if err := src.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return &multiError{errs: errs}
	}
}

// Join takes a slice of 1-channel sources and
// joins them into a single len(src)-channel source.
//
// If any of the sources has more than 1 channel,
// or if there is any difference in the sound codec
// or frequency of any of the sources, Join fails
// returning a nil source and a non-nil error.
//
// Sources which themselves carry more than one channel are accepted as
// long as every source carries the same channel count: their channels
// are concatenated in source order, so the resulting source has
// len(srcs)*srcs[0].Channels() channels.
//
// A single source is returned unchanged so that single-channel
// pass-through behavior is preserved.
func Join(srcs ...sound.Source) (sound.Source, error) {
	if len(srcs) == 0 {
		return nil, fmt.Errorf("zero channel source not allowed.")
	}
	if len(srcs) == 1 {
		return srcs[0], nil
	}
	if e := Compat(srcs...); e != nil {
		return nil, e
	}
	return &join{
		srcs:    srcs,
		done:    make([]bool, len(srcs)),
		closeCh: make(chan struct{}),
	}, nil
}

// MustJoin is like Join but it panics in case of an
// error.
func MustJoin(srcs ...sound.Source) sound.Source {
	res, e := Join(srcs...)
	if e != nil {
		panic(e.Error())
	}
	return res
}
