package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// ADTS frame helpers
// ---------------------------------------------------------------------------

type bitWriter struct {
	bits []uint8
}

func newBitWriter() *bitWriter { return &bitWriter{bits: make([]uint8, 0, 64)} }

func (w *bitWriter) write(n int, value uint32) {
	for i := n - 1; i >= 0; i-- {
		w.bits = append(w.bits, uint8((value>>uint(i))&1))
	}
}

func (w *bitWriter) bytes() []byte {
	out := make([]byte, (len(w.bits)+7)/8)
	for i, bit := range w.bits {
		if bit != 0 {
			out[i/8] |= 1 << uint(7-(i%8))
		}
	}
	return out
}

// adtsFrame builds an ADTS frame whose payload is END_ELEMENT + zero padding.
// frameLength is computed from the actual size, so the header always agrees
// with the body.
func adtsFrame(profile, sampleIndex, chanConfig int, protectAbsent bool) []byte {
	headerLen := 7
	if !protectAbsent {
		headerLen = 9
	}
	payload := []byte{0xE0} // END_ELEMENT + padding
	total := headerLen + len(payload)

	w := newBitWriter()
	w.write(12, 0xfff)
	w.write(1, 0) // MPEG-4
	w.write(2, 0) // layer
	if protectAbsent {
		w.write(1, 1)
	} else {
		w.write(1, 0)
	}
	w.write(2, uint32(profile-1))
	w.write(4, uint32(sampleIndex))
	w.write(1, 0)
	w.write(3, uint32(chanConfig))
	w.write(4, 0)
	w.write(13, uint32(total))
	w.write(11, 0)
	w.write(2, 0)
	if !protectAbsent {
		w.write(16, 0) // CRC
	}
	frame := w.bytes()
	return append(frame, payload...)
}

// adtsFrameWithBadLength builds an ADTS frame whose declared frame length is
// bogus (too long for the bytes that follow).
func adtsFrameWithBadLength(profile, sampleIndex, chanConfig, declaredLength int) []byte {
	w := newBitWriter()
	w.write(12, 0xfff)
	w.write(1, 0)
	w.write(2, 0)
	w.write(1, 1) // protection absent
	w.write(2, uint32(profile-1))
	w.write(4, uint32(sampleIndex))
	w.write(1, 0)
	w.write(3, uint32(chanConfig))
	w.write(4, 0)
	w.write(13, uint32(declaredLength))
	w.write(11, 0)
	w.write(2, 0)
	frame := w.bytes()
	return append(frame, 0xE0) // body shorter than declared
}

// corruptFrame builds a well-formed ADTS frame whose payload cannot be decoded.
//
// Payload 0x20 00 00 00 begins with a CPE element (type 1) whose common-window
// ICS runs out of bits, so decoding fails with an ErrInvalidFrame-class error
// rather than succeeding as silence. Holding the payload at a fixed size and
// varying only the header fields lets tests exercise corrupt frames that
// declare a different sample rate/channel count from the real audio.
func corruptFrame(profile, sampleIndex, chanConfig int) []byte {
	payload := []byte{0x20, 0x00, 0x00, 0x00}
	total := 7 + len(payload)

	w := newBitWriter()
	w.write(12, 0xfff)
	w.write(1, 0)
	w.write(2, 0)
	w.write(1, 1)
	w.write(2, uint32(profile-1))
	w.write(4, uint32(sampleIndex))
	w.write(1, 0)
	w.write(3, uint32(chanConfig))
	w.write(4, 0)
	w.write(13, uint32(total))
	w.write(11, 0)
	w.write(2, 0)
	frame := w.bytes()
	return append(frame, payload...)
}

// ---------------------------------------------------------------------------
// WAV parsing helper
// ---------------------------------------------------------------------------

type wavInfo struct {
	sampleRate  uint32
	numChannels uint16
	bitsPerSamp uint16
	dataSize    uint32
	payload     []byte
}

func parseWAV(t *testing.T, data []byte) wavInfo {
	t.Helper()
	if len(data) < 44 {
		t.Fatalf("WAV too short: %d bytes", len(data))
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("bad RIFF/WAVE header: %q %q", data[0:4], data[8:12])
	}
	if string(data[12:16]) != "fmt " {
		t.Fatalf("missing fmt chunk, got %q", data[12:16])
	}
	if string(data[36:40]) != "data" {
		t.Fatalf("missing data chunk, got %q", data[36:40])
	}
	return wavInfo{
		numChannels: binary.LittleEndian.Uint16(data[22:24]),
		sampleRate:  binary.LittleEndian.Uint32(data[24:28]),
		bitsPerSamp: binary.LittleEndian.Uint16(data[34:36]),
		dataSize:    binary.LittleEndian.Uint32(data[40:44]),
		payload:     data[44:],
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func writeTemp(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestCleanStreamProducesWAV(t *testing.T) {
	dir := t.TempDir()
	var stream []byte
	for i := 0; i < 4; i++ {
		stream = append(stream, adtsFrame(2, 4, 2, true)...)
	}
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	code := run([]string{"-v", in, out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	info := parseWAV(t, data)
	if info.sampleRate != 44100 {
		t.Errorf("sampleRate=%d want 44100", info.sampleRate)
	}
	if info.numChannels != 2 {
		t.Errorf("channels=%d want 2", info.numChannels)
	}
	if info.bitsPerSamp != 16 {
		t.Errorf("bitsPerSample=%d want 16", info.bitsPerSamp)
	}
	// 4 frames * 1024 samples * 2 channels * 2 bytes.
	wantSize := uint32(4 * 1024 * 2 * 2)
	if info.dataSize != wantSize {
		t.Errorf("dataSize=%d want %d", info.dataSize, wantSize)
	}
	if uint32(len(info.payload)) != wantSize {
		t.Errorf("payload=%d want %d", len(info.payload), wantSize)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("Bad frames:   0")) {
		t.Errorf("verbose output missing bad-frame count: %s", stderr.String())
	}
}

func TestBadFrameInMiddleIsSkipped(t *testing.T) {
	dir := t.TempDir()
	var stream []byte
	frame := adtsFrame(2, 4, 2, true)
	stream = append(stream, frame...)
	stream = append(stream, frame...)
	badOffset := len(stream)
	stream = append(stream, corruptFrame(2, 4, 2)...)
	stream = append(stream, frame...)
	stream = append(stream, frame...)
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	code := run([]string{"-v", in, out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	info := parseWAV(t, data)
	// Four valid frames survive.
	wantSize := uint32(4 * 1024 * 2 * 2)
	if info.dataSize != wantSize {
		t.Errorf("dataSize=%d want %d", info.dataSize, wantSize)
	}
	msg := stderr.String()
	if !bytes.Contains([]byte(msg), []byte("Bad frames:   1")) {
		t.Errorf("expected 1 bad frame, stderr=%s", msg)
	}
	if !bytes.Contains([]byte(msg), []byte("first at offset")) {
		t.Errorf("expected first bad offset, stderr=%s", msg)
	}
	_ = badOffset
}

func TestBadFrameFirstIsSkipped(t *testing.T) {
	dir := t.TempDir()
	var stream []byte
	badOffset := 0
	stream = append(stream, corruptFrame(2, 4, 2)...)
	frame := adtsFrame(2, 4, 2, true)
	stream = append(stream, frame...)
	stream = append(stream, frame...)
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	code := run([]string{"-v", in, out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	info := parseWAV(t, data)
	wantSize := uint32(2 * 1024 * 2 * 2)
	if info.dataSize != wantSize {
		t.Errorf("dataSize=%d want %d", info.dataSize, wantSize)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("first at offset 0")) {
		t.Errorf("expected first bad offset 0, stderr=%s", stderr.String())
	}
	_ = badOffset
}

func TestBadFrameLastIsSkipped(t *testing.T) {
	dir := t.TempDir()
	var stream []byte
	frame := adtsFrame(2, 4, 2, true)
	stream = append(stream, frame...)
	stream = append(stream, frame...)
	stream = append(stream, corruptFrame(2, 4, 2)...)
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	code := run([]string{"-v", in, out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	info := parseWAV(t, data)
	wantSize := uint32(2 * 1024 * 2 * 2)
	if info.dataSize != wantSize {
		t.Errorf("dataSize=%d want %d", info.dataSize, wantSize)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("Bad frames:   1")) {
		t.Errorf("expected 1 bad frame, stderr=%s", stderr.String())
	}
}

func TestBadFrameLengthDoesNotOverrun(t *testing.T) {
	dir := t.TempDir()
	var stream []byte
	frame := adtsFrame(2, 4, 2, true)
	stream = append(stream, frame...)
	// A header that declares more bytes than remain: must not read past EOF.
	stream = append(stream, adtsFrameWithBadLength(2, 4, 2, 200)...)
	stream = append(stream, frame...)
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	code := run([]string{"-v", in, out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	info := parseWAV(t, data)
	// The truncated/oversized frame and its trailing bytes are skipped; the
	// two valid frames remain.
	if info.dataSize == 0 {
		t.Fatalf("expected some decodable audio, got empty payload")
	}
	if !bytes.Contains(stderr.Bytes(), []byte("Bad frames:")) {
		t.Errorf("expected bad-frame report, stderr=%s", stderr.String())
	}
}

func TestAllBadFramesExitsNonZeroNoOutput(t *testing.T) {
	dir := t.TempDir()
	var stream []byte
	stream = append(stream, corruptFrame(2, 4, 2)...)
	stream = append(stream, corruptFrame(2, 4, 2)...)
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	code := run([]string{in, out}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit for all-bad input, stderr=%s", stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("output file must not be created when nothing decodes")
	}
}

func TestNoADTSExitsNonZeroNoOutput(t *testing.T) {
	dir := t.TempDir()
	in := writeTemp(t, dir, "in.aac", []byte("this is not aac data at all"))
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	code := run([]string{in, out}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit, stderr=%s", stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("output file must not be created")
	}
}

func TestWAVIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	var stream []byte
	frame := adtsFrame(2, 4, 2, true)
	stream = append(stream, frame...)
	stream = append(stream, corruptFrame(2, 4, 2)...)
	stream = append(stream, frame...)
	stream = append(stream, frame...)
	in := writeTemp(t, dir, "in.aac", stream)

	var outputs [][]byte
	for i := 0; i < 2; i++ {
		out := filepath.Join(dir, "out"+string(rune('a'+i))+".wav")
		var stdout, stderr bytes.Buffer
		if code := run([]string{in, out}, &stdout, &stderr); code != 0 {
			t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("reading %s: %v", out, err)
		}
		outputs = append(outputs, data)
	}
	if !bytes.Equal(outputs[0], outputs[1]) {
		t.Fatalf("repeated decodes differ (%d vs %d bytes)", len(outputs[0]), len(outputs[1]))
	}
}

func TestWAVHeaderMatchesPayloadMono(t *testing.T) {
	dir := t.TempDir()
	var stream []byte
	// Mono AAC-LC, sample index 3 = 48000 Hz.
	for i := 0; i < 3; i++ {
		stream = append(stream, adtsFrame(2, 3, 1, true)...)
	}
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	if code := run([]string{in, out}, &stdout, &stderr); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	info := parseWAV(t, data)
	if info.sampleRate != 48000 {
		t.Errorf("sampleRate=%d want 48000", info.sampleRate)
	}
	if info.numChannels != 1 {
		t.Errorf("channels=%d want 1", info.numChannels)
	}
	wantSize := uint32(3 * 1024 * 1 * 2)
	if info.dataSize != wantSize {
		t.Errorf("dataSize=%d want %d", info.dataSize, wantSize)
	}
	if uint32(len(info.payload)) != wantSize {
		t.Errorf("actual payload=%d want %d", len(info.payload), wantSize)
	}
}

func TestBadFramesDoNotChangeWAVFormat(t *testing.T) {
	// WAV format must come from real decoded data, not a corrupt frame's
	// declared header values.
	dir := t.TempDir()
	var stream []byte
	// Corrupt frame claiming mono 96 kHz.
	stream = append(stream, corruptFrame(2, 0, 1)...)
	// Real stereo 44.1 kHz frames.
	stream = append(stream, adtsFrame(2, 4, 2, true)...)
	stream = append(stream, adtsFrame(2, 4, 2, true)...)
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	if code := run([]string{in, out}, &stdout, &stderr); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	info := parseWAV(t, data)
	if info.sampleRate != 44100 {
		t.Errorf("sampleRate=%d want 44100", info.sampleRate)
	}
	if info.numChannels != 2 {
		t.Errorf("channels=%d want 2", info.numChannels)
	}
	wantSize := uint32(2 * 1024 * 2 * 2)
	if info.dataSize != wantSize {
		t.Errorf("dataSize=%d want %d", info.dataSize, wantSize)
	}
}

func TestMissingOutputPathErrors(t *testing.T) {
	dir := t.TempDir()
	stream := adtsFrame(2, 4, 2, true)
	in := writeTemp(t, dir, "in.aac", stream)
	var stdout, stderr bytes.Buffer
	if code := run([]string{in}, &stdout, &stderr); code == 0 {
		t.Fatal("expected error when output path missing")
	}
}

func TestRawModeStillWorks(t *testing.T) {
	dir := t.TempDir()
	var stream []byte
	stream = append(stream, adtsFrame(2, 4, 2, true)...)
	stream = append(stream, corruptFrame(2, 4, 2)...)
	stream = append(stream, adtsFrame(2, 4, 2, true)...)
	in := writeTemp(t, dir, "in.aac", stream)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--raw", in}, &stdout, &stderr); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
	}
	// Two valid stereo frames of int16 samples.
	wantLen := 2 * 1024 * 2 * 2
	if stdout.Len() != wantLen {
		t.Errorf("raw output length=%d want %d", stdout.Len(), wantLen)
	}
}

func TestConfigurationErrorIsFatal(t *testing.T) {
	// A frame that forces a configuration error (channel config 0 => PCE) must
	// abort with a non-zero exit rather than being reported as a skippable bad
	// frame. With no successful frame, no output file is produced.
	dir := t.TempDir()
	var stream []byte
	stream = append(stream, adtsFrame(2, 4, 0, true)...) // chanConfig 0
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	code := run([]string{in, out}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit for configuration error, stderr=%s", stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("output file must not be created on configuration error")
	}
}

func TestTruncatedTailIsSkipped(t *testing.T) {
	// A final frame cut short is a damaged region, not an error: the earlier
	// frames still produce a WAV.
	dir := t.TempDir()
	frame := adtsFrame(2, 4, 2, true)
	var stream []byte
	stream = append(stream, frame...)
	stream = append(stream, frame...)
	stream = append(stream, frame[:len(frame)-1]...) // truncated last frame
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-v", in, out}, &stdout, &stderr); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	info := parseWAV(t, data)
	wantSize := uint32(2 * 1024 * 2 * 2)
	if info.dataSize != wantSize {
		t.Errorf("dataSize=%d want %d", info.dataSize, wantSize)
	}
}

func TestBadFramesBeforeFirstValidDoNotLockFormat(t *testing.T) {
	// Two undecodable frames declaring mono 96 kHz precede real stereo 44.1 kHz
	// audio. The output must be described by the valid audio, and the first bad
	// offset must be reported.
	dir := t.TempDir()
	var stream []byte
	stream = append(stream, corruptFrame(2, 0, 1)...)
	stream = append(stream, corruptFrame(2, 0, 1)...)
	stream = append(stream, adtsFrame(2, 4, 2, true)...)
	stream = append(stream, adtsFrame(2, 4, 2, true)...)
	in := writeTemp(t, dir, "in.aac", stream)
	out := filepath.Join(dir, "out.wav")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-v", in, out}, &stdout, &stderr); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	info := parseWAV(t, data)
	if info.sampleRate != 44100 {
		t.Errorf("sampleRate=%d want 44100", info.sampleRate)
	}
	if info.numChannels != 2 {
		t.Errorf("channels=%d want 2", info.numChannels)
	}
	if want := uint32(2 * 1024 * 2 * 2); info.dataSize != want {
		t.Errorf("dataSize=%d want %d", info.dataSize, want)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("Bad frames:   2")) {
		t.Errorf("expected 2 bad frames, stderr=%s", stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("first at offset 0")) {
		t.Errorf("expected first bad offset 0, stderr=%s", stderr.String())
	}
}
