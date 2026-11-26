// Copyright 2024 Jelly Terra
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0
// that can be found in the LICENSE file and https://mozilla.org/MPL/2.0/.

package main

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Realistic RTMP media payloads: AVC video (0x17 keyframe, codec 7) and AAC
// audio (0xAF, format 10). These are the codecs go-flv fully consumes on decode.
var testVideoPayload = []byte{
	0x17,                   // keyframe + AVC
	0x01, 0x00, 0x00, 0x00, // AVCPacketType=NALU, composition time 0
	0x65, 0x88, 0x80, 0x40, // (placeholder NAL bytes)
	0xAA, 0xBB, 0xCC, 0xDD,
}
var testAudioPayload = []byte{
	0xAF,       // AAC, 44kHz, 16-bit, stereo
	0x01,       // AACPacketType=raw
	0x12, 0x34, // (placeholder AAC frame bytes)
}

func writeSomeMedia(t *testing.T, f *FlvFile, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := f.WriteVideo(uint32(i*40), testVideoPayload); err != nil {
			t.Fatalf("unexpected video write error: %v", err)
		}
		if err := f.WriteAudio(uint32(i*40), testAudioPayload); err != nil {
			t.Fatalf("unexpected audio write error: %v", err)
		}
	}
}

// parseFlvStrict walks the file exactly as a standards-compliant FLV parser
// would: 9-byte header, leading PreviousTagSize0, then repeated
// (PreviousTagSize, 11-byte tag header, declared data size) units. It returns
// the number of complete tags and the byte size consumed. Any inconsistency
// (bad signature, mismatched PreviousTagSize, truncated tag, trailing garbage)
// is reported as an error; a clean EOF right after a PreviousTagSize is success.
func parseFlvStrict(t *testing.T, path string) (int, int64) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("open recording: %v", err)
	}
	r := newByteCursor(data)

	if r.len() < 9 || string(r.take(3)) != "FLV" {
		t.Fatalf("missing or bad FLV signature")
	}
	version := r.take(1)[0]
	flags := r.take(1)[0]
	dataOffset := binary.BigEndian.Uint32(r.take(4))
	if version != 1 {
		t.Fatalf("unexpected FLV version: %d", version)
	}
	if flags&0b101 == 0 {
		t.Fatalf("FLV flags declare neither audio nor video: %#x", flags)
	}
	if dataOffset != 9 {
		t.Fatalf("unexpected FLV data offset: %d", dataOffset)
	}

	// PreviousTagSize0 is written by the first tag encode, so a recording that
	// never received media may contain just the 9-byte header: a valid empty FLV.
	if r.len() == 0 {
		return 0, int64(len(data))
	}
	prevSizeBytes := r.take(4)
	if len(prevSizeBytes) != 4 {
		t.Fatalf("recording truncated before PreviousTagSize0, have %d bytes", len(data))
	}
	if got := binary.BigEndian.Uint32(prevSizeBytes); got != 0 {
		t.Fatalf("PreviousTagSize0 must be 0, got %d", got)
	}

	tags := 0
	for r.len() > 0 {
		// Each unit is: 11-byte tag header, body, then 4-byte PreviousTagSize.
		hdr := r.take(11)
		if len(hdr) != 11 {
			t.Fatalf("tag %d: truncated tag header", tags)
		}
		tagType := hdr[0]
		dataSize := uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3])
		if tagType != 8 && tagType != 9 && tagType != 18 {
			t.Fatalf("tag %d: unsupported tag type %d (stream misaligned or corrupt)", tags, tagType)
		}

		body := r.take(int(dataSize))
		if len(body) != int(dataSize) {
			t.Fatalf("tag %d: truncated body, want %d bytes", tags, dataSize)
		}

		// The trailing PreviousTagSize must equal the tag's on-wire size.
		wantSize := uint32(11 + dataSize)
		if r.len() < 4 {
			t.Fatalf("tag %d: missing trailing PreviousTagSize", tags)
		}
		if got := binary.BigEndian.Uint32(r.take(4)); got != wantSize {
			t.Fatalf("tag %d: PreviousTagSize mismatch: want %d, got %d",
				tags, wantSize, got)
		}

		tags++
	}

	return tags, int64(len(data))
}

type byteCursor struct {
	data []byte
	pos  int
}

func newByteCursor(data []byte) *byteCursor { return &byteCursor{data: data} }

func (c *byteCursor) len() int { return len(c.data) - c.pos }

func (c *byteCursor) take(n int) []byte {
	if c.pos+n > len(c.data) {
		// Return whatever remains; callers compare lengths to detect truncation.
		rest := c.data[c.pos:]
		c.pos = len(c.data)
		return rest
	}
	b := c.data[c.pos : c.pos+n]
	c.pos += n
	return b
}

// A normally closed recording is fully readable by an FLV parser.
func TestFlvRecordingPlayableAfterNormalClose(t *testing.T) {
	dir := t.TempDir()
	f, err := CreateFlvFile(dir, "normal")
	if err != nil {
		t.Fatalf("create flv: %v", err)
	}

	writeSomeMedia(t, f, 10)
	f.Close()
	f.Close() // repeated close must be a no-op

	tags, _ := parseFlvStrict(t, filepath.Join(dir, "normal.flv"))
	if tags != 20 { // 10 video + 10 audio
		t.Fatalf("expected 20 complete tags, got %d", tags)
	}
}

// flakyBackend fails writes after a small budget, simulating a disk-full / I/O
// error halfway through a tag.
type flakyBackend struct {
	file      *os.File
	remaining int
}

func (f *flakyBackend) Write(p []byte) (int, error) {
	if f.remaining <= 0 {
		return 0, errors.New("simulated disk I/O failure")
	}

	if len(p) > f.remaining {
		p = p[:f.remaining]
		n, _ := f.file.Write(p)
		f.remaining -= n
		return n, errors.New("simulated partial write failure")
	}

	n, err := f.file.Write(p)
	f.remaining -= n
	return n, err
}

func (f *flakyBackend) Truncate(size int64) error { return f.file.Truncate(size) }
func (f *flakyBackend) Sync() error               { return f.file.Sync() }
func (f *flakyBackend) Close() error              { return f.file.Close() }

// A torn last tag must not corrupt the recording: close truncates back to the
// last fully written tag boundary and the file still parses end to end.
func TestFlvRecordingTruncatesTornTagAndStaysPlayable(t *testing.T) {
	dir := t.TempDir()

	raw, err := os.Create(filepath.Join(dir, "torn.flv"))
	if err != nil {
		t.Fatalf("create raw file: %v", err)
	}

	f, err := newFlvFile("recording:torn.flv", raw)
	if err != nil {
		t.Fatalf("new flv file: %v", err)
	}

	// 5 good media pairs, then swap the backend for a flaky one that dies
	// inside the next tag.
	writeSomeMedia(t, f, 5)

	goodBoundary := f.file.safeOffset

	f.file.backend = &flakyBackend{file: raw, remaining: 7} // dies mid-tag

	sawError := false
	for i := 0; i < 5; i++ {
		if err := f.WriteVideo(1000, testVideoPayload); err != nil {
			sawError = true
			break
		}
	}
	if !sawError {
		t.Fatal("expected a write failure to be surfaced")
	}

	f.Close()

	tags, size := parseFlvStrict(t, filepath.Join(dir, "torn.flv"))
	if tags != 10 { // only the 5 good pairs survive
		t.Fatalf("expected 10 intact tags after truncation, got %d", tags)
	}
	if size != goodBoundary {
		t.Fatalf("file must be truncated to safe boundary %d, actual size %d", goodBoundary, size)
	}
}

// Close must never double-close the underlying file even under concurrent calls.
func TestFlvCloseIdempotentConcurrent(t *testing.T) {
	dir := t.TempDir()
	f, err := CreateFlvFile(dir, "concurrent")
	if err != nil {
		t.Fatalf("create flv: %v", err)
	}
	writeSomeMedia(t, f, 3)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			f.Close()
		}
	}()
	for i := 0; i < 50; i++ {
		f.Close()
	}
	<-done

	tags, _ := parseFlvStrict(t, filepath.Join(dir, "concurrent.flv"))
	if tags != 6 {
		t.Fatalf("expected 6 complete tags, got %d", tags)
	}
}
