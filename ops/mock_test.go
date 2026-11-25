// Copyright 2018 The ZikiChombo Authors. All rights reserved.  Use of this source
// code is governed by a license that can be found in the License file.

package ops

import (
	"errors"
	"io"
	"sync"

	"github.com/zikichombo/sound"
	"github.com/zikichombo/sound/freq"
)

// errMockClose is returned by mockSource.Close to test error aggregation.
var errMockClose = errors.New("mock: close failed")

// mockSource is a thread-safe, controllable sound.SourceSeeker used by
// the join/loop tests.
//
// Data is stored deinterleaved: dat holds nChans blocks of len/nChans
// frames each.  pos is measured in frames.  A source can be configured
// to fail Receive after a number of reads, to fail Seek, or to block on
// Receive until released, which is needed to exercise closing an
// infinite loop.
type mockSource struct {
	mu      sync.Mutex
	dat     []float64
	nChans  int
	rate    freq.T
	pos     int
	closed  bool
	closeMu sync.Mutex
	closeCh chan struct{}

	// closeErr, if non-nil, is returned by Close.
	closeErr error
	// recvErr, if non-nil, is returned by Receive instead of data once
	// failAfter reads have been served.
	recvErr error
	// failAfter limits the number of successful Reads before recvErr is
	// returned.  A negative value means never fail.
	failAfter int
	reads     int

	// seekErr, if non-nil, is returned by Seek.
	seekErr error

	// block, when non-nil, makes Receive wait on it before serving data.
	block chan struct{}
}

func newMockSource(dat []float64, nChans int, rate freq.T) *mockSource {
	if len(dat)%nChans != 0 {
		panic("mockSource: channel alignment")
	}
	return &mockSource{
		dat:       dat,
		nChans:    nChans,
		rate:      rate,
		failAfter: -1,
		closeCh:   make(chan struct{}),
	}
}

func (m *mockSource) Channels() int { return m.nChans }

func (m *mockSource) SampleRate() freq.T { return m.rate }

func (m *mockSource) Len() int64 { return int64(len(m.dat) / m.nChans) }

func (m *mockSource) Pos() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(m.pos)
}

func (m *mockSource) Seek(f int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seekErr != nil {
		return m.seekErr
	}
	if f < 0 || f > m.Len() {
		return errors.New("mock: seek out of range")
	}
	m.pos = int(f)
	return nil
}

func (m *mockSource) Receive(dst []float64) (int, error) {
	if m.block != nil {
		// Wait until the test explicitly releases the read, or until
		// Close unblocks a stuck reader.
		select {
		case <-m.block:
		case <-m.closeCh:
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(dst)%m.nChans != 0 {
		return 0, sound.ErrChannelAlignment
	}
	if m.closed {
		return 0, io.EOF
	}
	if m.recvErr != nil && m.reads >= m.failAfter {
		return 0, m.recvErr
	}

	nF := len(dst) / m.nChans
	total := len(m.dat) / m.nChans
	if m.pos >= total {
		return 0, io.EOF
	}
	n := total - m.pos
	if n > nF {
		n = nF
	}
	for c := 0; c < m.nChans; c++ {
		copy(dst[c*nF:c*nF+n], m.dat[c*total+m.pos:c*total+m.pos+n])
	}
	m.pos += n
	m.reads++
	return n, nil
}

func (m *mockSource) Close() error {
	// Signal any reader blocked in Receive before taking the data lock,
	// so Close cannot deadlock against an in-flight Receive.
	m.closeMu.Lock()
	if !m.closed {
		close(m.closeCh)
	}
	m.closeMu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	return m.closeErr
}
