package xm

// This file contains test-only helpers that build small but valid
// xmfile.Module objects. These modules are compiled and played with
// the stream/synthesizer APIs, so the tests don't rely on any
// real audio device or external .xm files.

import (
	"io"
	"testing"

	"github.com/quasilyte/xm/xmfile"
)

// testNoteSpec describes a single pattern cell.
type testNoteSpec struct {
	note       uint8
	instrument uint8
	volume     uint8
	effect     uint8
	param      uint8
}

// testModuleBuilder constructs xmfile.Module values.
//
// Note IDs are interned, so the same spec always maps to the same note ID.
// Note ID 0 is reserved for a completely empty note.
type testModuleBuilder struct {
	numChannels int
	bpm         int
	tempo       int

	notes   []xmfile.PatternNote
	noteIDs map[testNoteSpec]uint16

	patterns    []xmfile.Pattern
	instruments []xmfile.Instrument
}

func newTestModuleBuilder(numChannels int) *testModuleBuilder {
	return &testModuleBuilder{
		numChannels: numChannels,
		bpm:         120,
		tempo:       4,
		notes:       []xmfile.PatternNote{{}}, // ID 0: an empty note
		noteIDs:     make(map[testNoteSpec]uint16),
	}
}

func (b *testModuleBuilder) setBPM(bpm int) *testModuleBuilder {
	b.bpm = bpm
	return b
}

func (b *testModuleBuilder) setTempo(tempo int) *testModuleBuilder {
	b.tempo = tempo
	return b
}

// addInstrument appends an instrument and returns its 1-based ID
// (as it's used inside xmfile.PatternNote).
func (b *testModuleBuilder) addInstrument(inst xmfile.Instrument) int {
	b.instruments = append(b.instruments, inst)
	return len(b.instruments)
}

// addPattern appends a pattern. Every row is padded with empty notes
// up to the module channel count.
func (b *testModuleBuilder) addPattern(rows ...[]testNoteSpec) {
	var pat xmfile.Pattern
	for _, row := range rows {
		ids := make([]uint16, b.numChannels)
		for ch := 0; ch < b.numChannels; ch++ {
			var spec testNoteSpec
			if ch < len(row) {
				spec = row[ch]
			}
			ids[ch] = b.internNote(spec)
		}
		pat.Rows = append(pat.Rows, xmfile.PatternRow{Notes: ids})
	}
	b.patterns = append(b.patterns, pat)
}

func (b *testModuleBuilder) internNote(spec testNoteSpec) uint16 {
	if id, ok := b.noteIDs[spec]; ok {
		return id
	}
	id := uint16(len(b.notes))
	b.notes = append(b.notes, xmfile.PatternNote{
		ID:              id,
		Note:            spec.note,
		Instrument:      spec.instrument,
		Volume:          spec.volume,
		EffectType:      spec.effect,
		EffectParameter: spec.param,
	})
	b.noteIDs[spec] = id
	return id
}

func (b *testModuleBuilder) build() *xmfile.Module {
	order := make([]uint8, len(b.patterns))
	for i := range order {
		order[i] = uint8(i)
	}
	m := &xmfile.Module{
		Name:           "test-module",
		Version:        [2]byte{1, 4},
		SongLength:     len(b.patterns),
		NumChannels:    b.numChannels,
		NumPatterns:    len(b.patterns),
		NumInstruments: len(b.instruments),
		Flags:          1, // linear frequency table
		DefaultTempo:   b.tempo,
		DefaultBPM:     b.bpm,
		PatternOrder:   order,
		Patterns:       b.patterns,
		Notes:          b.notes,
		Instruments:    b.instruments,
	}
	return m
}

// makeTestInstrument returns a single-sample looping instrument.
// The sample data is an alternating square wave with the specified amplitude.
func makeTestInstrument(amplitude int8, volume, panning uint8, length int) xmfile.Instrument {
	data := make([]uint8, length)
	prev := int8(0)
	for i := 0; i < length; i++ {
		v := amplitude
		if i%2 != 0 {
			v = -amplitude
		}
		data[i] = uint8(v - prev)
		prev = v
	}
	return xmfile.Instrument{
		Samples: []xmfile.InstrumentSample{{
			Length:       length,
			LoopStart:    0,
			LoopLength:   length,
			Volume:       int(volume),
			Panning:      panning,
			TypeFlags:    1, // forward loop
			Format:       xmfile.SampleFormatDeltaPacked,
			Data:         data,
			RelativeNote: 0,
		}},
	}
}

// makeTestInstrumentWithVolumeEnvelope is like makeTestInstrument, but it
// also enables a descending volume envelope.
func makeTestInstrumentWithVolumeEnvelope(amplitude int8, volume, panning uint8, length int) xmfile.Instrument {
	inst := makeTestInstrument(amplitude, volume, panning, length)
	inst.EnvelopeVolume = []xmfile.EnvelopePoint{
		{X: 0, Y: 64},
		{X: 4, Y: 32},
		{X: 8, Y: 0},
	}
	inst.VolumeFlags = 1 // enabled, but not sustaining/looping
	inst.VolumeSustainPoint = 0
	inst.VolumeLoopStartPoint = 0
	inst.VolumeLoopEndPoint = 0
	return inst
}

// renderTicks reads the given amount of whole ticks from the stream and
// returns the produced PCM bytes. It fails the test if Read can't produce
// the requested amount of data.
func renderTicks(t *testing.T, s *Stream, numTicks int) []byte {
	t.Helper()
	bytesPerTick := int(s.GetInfo().BytesPerTick)
	if bytesPerTick == 0 {
		t.Fatalf("stream has zero bytes per tick")
	}
	buf := make([]byte, numTicks*bytesPerTick+4)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatalf("read %d ticks: %v", numTicks, err)
	}
	if n != numTicks*bytesPerTick {
		t.Fatalf("read %d ticks: got %d bytes, want %d", numTicks, n, numTicks*bytesPerTick)
	}
	return buf[:n]
}

// renderSynthTicks reads up to numTicks whole ticks from a synthesizer
// stream. A synthesizer trial only has a single-row pattern, so the read
// may legitimately end with io.EOF earlier than requested; the bytes
// produced before that point are returned.
func renderSynthTicks(t *testing.T, s *Synthesizer, numTicks int) []byte {
	t.Helper()
	bytesPerTick := int(s.stream.GetInfo().BytesPerTick)
	if bytesPerTick == 0 {
		t.Fatalf("stream has zero bytes per tick")
	}
	buf := make([]byte, numTicks*bytesPerTick+4)
	n, err := s.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read %d ticks: %v", numTicks, err)
	}
	if n == 0 {
		t.Fatalf("read %d ticks: no data produced", numTicks)
	}
	if n%bytesPerTick != 0 {
		t.Fatalf("read %d ticks: produced %d bytes, not a whole number of %d-byte ticks",
			numTicks, n, bytesPerTick)
	}
	return buf[:n]
}

// renderSynthWholeTrial renders the whole prepared trial: it reads until
// the single-row pattern of a synthesizer reaches EOF.
func renderSynthWholeTrial(t *testing.T, s *Synthesizer) []byte {
	t.Helper()
	bytesPerTick := int(s.stream.GetInfo().BytesPerTick)
	buf := make([]byte, 16*bytesPerTick+4)
	n, err := s.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read trial: %v", err)
	}
	if n == 0 {
		t.Fatalf("read trial: no data produced")
	}
	if n%bytesPerTick != 0 {
		t.Fatalf("read trial: produced %d bytes, not a whole number of %d-byte ticks",
			n, bytesPerTick)
	}
	return buf[:n]
}

// mustLoadModule loads m into a fresh stream with the given config.
// renderUntilEOF reads the whole stream and returns the produced PCM
// bytes. It works with modules that change the BPM/tempo during playback,
// since the tick size may change mid-stream. An io.EOF result is expected
// and is not considered an error.
func renderUntilEOF(t *testing.T, s *Stream) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 8192)
	for {
		n, err := s.Read(buf)
		if n != 0 {
			out = append(out, buf[:n]...)
		}
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if n == 0 {
			t.Fatalf("read made no progress")
		}
	}
}

func mustLoadModule(t *testing.T, m *xmfile.Module, config LoadModuleConfig) *Stream {
	t.Helper()
	s := NewStream()
	if err := s.LoadModule(m, config); err != nil {
		t.Fatalf("load module: %v", err)
	}
	return s
}

// assertBytesEqual fails the test if the two PCM buffers differ.
func assertBytesEqual(t *testing.T, label string, got, want []byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: PCM length mismatch: got %d, want %d", label, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: PCM differs at byte %d: got %d, want %d",
				label, i, got[i], want[i])
		}
	}
}
