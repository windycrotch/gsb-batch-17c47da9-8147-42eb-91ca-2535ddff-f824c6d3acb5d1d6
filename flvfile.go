// Copyright 2024 Jelly Terra
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0
// that can be found in the LICENSE file and https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"github.com/yutopp/go-flv"
	flvtag "github.com/yutopp/go-flv/tag"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// flvBackend is the storage underneath a recording. *os.File satisfies it in
// production; tests can substitute a failing implementation to inject I/O
// errors.
type flvBackend interface {
	io.Writer
	Truncate(size int64) error
	Sync() error
	Close() error
}

// safeFile tracks the offset up to which the backend contains only fully
// written FLV data. A "unit" groups the 4-byte PreviousTagSize and the FLV tag
// body that go-flv writes back to back: the safe boundary only advances when
// every byte of the unit was written. A torn tag (disk full, I/O error, ...) is
// thus never committed. On close the backend is truncated back to the safe
// boundary, which keeps the recording readable by a strict FLV parser even when
// the session ended abnormally.
type safeFile struct {
	mu      sync.Mutex
	backend flvBackend

	// safeOffset points right after the last fully written data.
	safeOffset int64

	// Unit bookkeeping. Between BeginUnit and EndUnit, writes accumulate on
	// pendingOffset instead of safeOffset. EndUnit commits only when the whole
	// unit succeeded.
	inUnit     bool
	unitStart  int64
	pendingOff int64
	unitFailed bool
}

func (s *safeFile) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, err := s.backend.Write(p)
	if err != nil {
		if s.inUnit {
			s.unitFailed = true
			s.pendingOff = s.unitStart
		}
		return n, err
	}

	if s.inUnit {
		s.pendingOff += int64(n)
	} else {
		s.safeOffset += int64(n)
	}
	return n, nil
}

func (s *safeFile) BeginUnit() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.inUnit = true
	s.unitStart = s.safeOffset
	s.pendingOff = s.safeOffset
	s.unitFailed = false
}

// EndUnit commits the pending bytes when the unit completed without error.
func (s *safeFile) EndUnit() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.inUnit && !s.unitFailed {
		s.safeOffset = s.pendingOff
	}
	s.inUnit = false
}

type FlvFile struct {
	label string
	file  *safeFile
	Enc   *flv.Encoder

	closeOnce sync.Once
	closeErr  error
}

func CreateFlvFile(baseDir, filename string) (*FlvFile, error) {
	path := filepath.Join(baseDir, filename+".flv")

	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	flv, err := newFlvFile("recording:"+filepath.Base(path), f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	return flv, nil
}

// newFlvFile wraps an already opened backend as a recording. It is shared by
// the production constructor and by failure-injection tests.
func newFlvFile(label string, backend flvBackend) (*FlvFile, error) {
	sf := &safeFile{backend: backend}

	enc, err := flv.NewEncoder(sf, flv.FlagsAudio|flv.FlagsVideo)
	if err != nil {
		return nil, err
	}

	return &FlvFile{
		label: label,
		file:  sf,
		Enc:   enc,
	}, nil
}

func (e *FlvFile) Label() string { return e.label }

// FatalOnWriteFailure marks the recording as a session-critical output: any
// write failure aborts the whole session instead of being isolated.
func (e *FlvFile) FatalOnWriteFailure() bool { return true }

// Close finalizes the recording exactly once. The backend is truncated to the
// last fully written FLV tag boundary, synced and then closed, so it always
// remains a playable FLV regardless of how the session terminated.
func (e *FlvFile) Close() {
	e.closeOnce.Do(func() {
		e.file.mu.Lock()
		defer e.file.mu.Unlock()

		// Drop any torn trailing data left by a failed write.
		if err := e.file.backend.Truncate(e.file.safeOffset); err != nil {
			e.closeErr = err
		}
		if err := e.file.backend.Sync(); err != nil {
			e.closeErr = err
		}
		if err := e.file.backend.Close(); err != nil {
			e.closeErr = err
		}
	})
}

// encodeTagUnit makes flv.Encoder's two back-to-back writes (PreviousTagSize
// and the tag body) atomic with respect to the safe boundary.
func (e *FlvFile) encodeTagUnit(tag *flvtag.FlvTag) error {
	e.file.BeginUnit()
	err := e.Enc.Encode(tag)
	e.file.EndUnit()
	return err
}

func (e *FlvFile) WriteAudio(timestamp uint32, payload []byte) error {
	var data flvtag.AudioData
	if err := flvtag.DecodeAudioData(bytes.NewReader(payload), &data); err != nil {
		return err
	}

	return e.encodeTagUnit(&flvtag.FlvTag{
		TagType:   flvtag.TagTypeAudio,
		Timestamp: timestamp,
		Data:      &data,
	})
}

func (e *FlvFile) WriteVideo(timestamp uint32, payload []byte) error {
	var data flvtag.VideoData
	if err := flvtag.DecodeVideoData(bytes.NewReader(payload), &data); err != nil {
		return err
	}

	return e.encodeTagUnit(&flvtag.FlvTag{
		TagType:   flvtag.TagTypeVideo,
		Timestamp: timestamp,
		Data:      &data,
	})
}

func (e *FlvFile) WriteSetFrame(timestamp uint32, data *rtmpmsg.DataMessage) error {
	return nil
}
