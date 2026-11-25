package xm

import (
	"testing"

	"github.com/quasilyte/xm/xmfile"
)

// These tests cover the synthesizer trial-playback behavior.
//
// They never touch a real audio device: the generated PCM data is
// inspected in memory.

func testSynthModule() *xmfile.Module {
	b := newTestModuleBuilder(2)
	// Instrument 1: loud, centred.
	b.addInstrument(makeTestInstrument(120, 64, 128, 4000))
	// Instrument 2: quiet, panned right, with a descending volume envelope.
	b.addInstrument(makeTestInstrumentWithVolumeEnvelope(30, 12, 220, 4000))
	b.addPattern(
		[]testNoteSpec{{note: 1 + 48, instrument: 1}},
		[]testNoteSpec{{note: 1 + 52, instrument: 2}},
	)
	return b.build()
}

func newTestSynth(t *testing.T, numChannels int) *Synthesizer {
	t.Helper()
	s := NewSynthesizer(SynthesizerConfig{NumChannels: numChannels})
	if err := s.LoadInstruments(testSynthModule(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load instruments: %v", err)
	}
	return s
}

func newInstrumentNote(note, inst uint8) xmfile.PatternNote {
	return xmfile.PatternNote{Note: note, Instrument: inst}
}

func assertSilence(t *testing.T, label string, pcm []byte) {
	t.Helper()
	for i, b := range pcm {
		if b != 0 {
			t.Fatalf("%s: expected silence, found non-zero byte %d at offset %d", label, b, i)
		}
	}
}

// TestSynthRepeatedPlayNoteIsDeterministic checks that playing the same
// note twice yields the same PCM, even if the previous trial used
// different instruments and different per-channel state.
func TestSynthRepeatedPlayNoteIsDeterministic(t *testing.T) {
	for _, numChannels := range []int{1, 2} {
		s := newTestSynth(t, numChannels)
		t.Run("", func(t *testing.T) {
			// First trial: instrument 1.
			if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
				t.Fatalf("play note: %v", err)
			}
			s.Rewind()
			first := renderSynthTicks(t, s, 4)

			// Dirty the trial state with a different instrument and more notes.
			if err := s.PlayNote(0.05, newInstrumentNote(53, 2), newInstrumentNote(60, 2)); err != nil {
				t.Fatalf("play note: %v", err)
			}
			s.Rewind()
			renderSynthTicks(t, s, 4)

			// Second trial: back to instrument 1. Must match the first trial.
			if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
				t.Fatalf("play note: %v", err)
			}
			s.Rewind()
			second := renderSynthTicks(t, s, 4)

			assertBytesEqual(t, "repeated PlayNote output", second, first)
		})
	}
}

// TestSynthVolumeEnvelopeDoesNotLeak checks that an instrument with a
// volume envelope doesn't affect the next trial of an instrument
// without an envelope.
func TestSynthVolumeEnvelopeDoesNotLeak(t *testing.T) {
	s := newTestSynth(t, 1)

	// Reference: play instrument 1 on a brand new synthesizer.
	ref := newTestSynth(t, 1)
	if err := ref.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	want := renderSynthTicks(t, ref, 8)

	// Play the enveloped instrument first, then instrument 1.
	if err := s.PlayNote(0.05, newInstrumentNote(53, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	renderSynthTicks(t, s, 8)

	if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	got := renderSynthTicks(t, s, 8)
	assertBytesEqual(t, "envelope leak", got, want)
}

// TestSynthNotesUseChannelsInOrder checks that notes are assigned to the
// channels in the given order and that extra notes are ignored.
func TestSynthNotesUseChannelsInOrder(t *testing.T) {
	s := newTestSynth(t, 2)
	if err := s.PlayNote(0.05, newInstrumentNote(53, 1), newInstrumentNote(60, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}

	// Both notes must be assigned, channel 0 first, channel 1 second.
	if s.stream.module.noteTab[0].inst == nil {
		t.Fatalf("channel 0 has no instrument")
	}
	if s.stream.module.noteTab[1].inst == nil {
		t.Fatalf("channel 1 has no instrument")
	}
	if s.stream.module.noteTab[0].inst == s.stream.module.noteTab[1].inst {
		t.Fatalf("expected distinct instruments in distinct channels")
	}
	// The note passed first must land in the first channel.
	if id0, id1 := s.stream.module.noteTab[0].inst.id, s.stream.module.noteTab[1].inst.id; id0 != 0 || id1 != 1 {
		t.Fatalf("notes are not in the given order: channel ids are %d, %d", id0, id1)
	}
	// Same for the notes' pitch (channel 0 => 53, channel 1 => 60).
	if s.stream.module.noteTab[0].raw != 53 || s.stream.module.noteTab[1].raw != 60 {
		t.Fatalf("notes are not in the given order: raw notes are %v, %v",
			s.stream.module.noteTab[0].raw, s.stream.module.noteTab[1].raw)
	}

	// Too many notes: they must be ignored, not panic.
	if err := s.PlayNote(0.05,
		newInstrumentNote(53, 1),
		newInstrumentNote(60, 2),
		newInstrumentNote(65, 1),
		newInstrumentNote(70, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	if len(s.stream.module.noteTab) != len(s.stream.channels) {
		t.Fatalf("note tab size mismatch: got %d, want %d",
			len(s.stream.module.noteTab), len(s.stream.channels))
	}

	// And the output must not panic when reading it.
	renderSynthTicks(t, s, 4)

	// A 1-channel synthesizer must only use 1 note even if more are passed.
	oneCh := newTestSynth(t, 1)
	if err := oneCh.PlayNote(0.05,
		newInstrumentNote(53, 1), newInstrumentNote(60, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	if len(oneCh.stream.module.noteTab) != 1 {
		t.Fatalf("1-channel synth note tab size mismatch: %d", len(oneCh.stream.module.noteTab))
	}
	renderSynthTicks(t, oneCh, 4)
}

// TestSynthDoesNotReadInvalidSamples checks that switching instruments
// never reads samples outside of the instrument's sample data.
func TestSynthDoesNotReadInvalidSamples(t *testing.T) {
	// Instrument 1 has a 4000-sample buffer, instrument 2 has a short one.
	b := newTestModuleBuilder(2)
	b.addInstrument(makeTestInstrument(120, 64, 128, 4000))
	b.addInstrument(makeTestInstrument(120, 64, 128, 8))
	mod := b.build()

	s := NewSynthesizer(SynthesizerConfig{NumChannels: 1})
	if err := s.LoadInstruments(mod, LoadModuleConfig{}); err != nil {
		t.Fatalf("load instruments: %v", err)
	}

	// Play a long trial with the large instrument to advance the sample
	// offset far beyond the small instrument's buffer size.
	if err := s.PlayNote(0.2, newInstrumentNote(53, 1)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	renderSynthTicks(t, s, 64)

	// Now play the short instrument. It must start from its own offset 0
	// and stay inside its buffer; a stale sample offset would point past it.
	if err := s.PlayNote(0.2, newInstrumentNote(53, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}

	for _, ch := range s.stream.channels {
		if ch.inst == nil {
			continue
		}
		if int(ch.sampleOffset) >= len(ch.inst.samples) {
			t.Fatalf("channel sample offset %v is outside the sample buffer (%d)",
				ch.sampleOffset, len(ch.inst.samples))
		}
	}
	renderSynthTicks(t, s, 64)
}

// TestSynthFailedPlayNoteLeavesSilence checks that a compilation error
// in PlayNote doesn't leave a half-built trial behind.
func TestSynthFailedPlayNoteLeavesSilence(t *testing.T) {
	s := newTestSynth(t, 2)

	// A note with an unsupported effect (0x1B is Rxy) should fail.
	bad := xmfile.PatternNote{
		Note:            53,
		Instrument:      1,
		EffectType:      0x1B,
		EffectParameter: 0,
	}
	if err := s.PlayNote(0.05, bad); err == nil {
		t.Fatalf("expected an error for an unsupported effect")
	}

	// Reading after the failed trial must be safe and silent.
	pcm := renderSynthTicks(t, s, 4)
	assertSilence(t, "after failed PlayNote", pcm)

	// A following valid trial must still work.
	if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
		t.Fatalf("play note after failure: %v", err)
	}
	renderSynthTicks(t, s, 4)
}

// TestSynthSeekAndRewindReset checks Seek/Rewind/GetInfo consistency
// after repeated trials.
func TestSynthSeekAndRewindReset(t *testing.T) {
	s := newTestSynth(t, 2)
	if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	first := renderSynthTicks(t, s, 4)

	pos, err := s.Seek(0, 0) // io.SeekStart + offset 0 == Rewind
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if pos != 0 {
		t.Fatalf("seek start returned %d, want 0", pos)
	}
	if cur, err := s.Seek(0, 1); err != nil || cur != 0 {
		t.Fatalf("seek current after rewind: %d, %v", cur, err)
	}

	second := renderSynthTicks(t, s, 4)
	assertBytesEqual(t, "synth playback after rewind", second, first)
}

// TestSynthSetInstrumentsResetsRuntime checks that SetInstruments
// (the GetInstruments/SetInstruments round-trip) also rewinds the
// runtime state and doesn't share per-trial tables.
func TestSynthSetInstrumentsResetsRuntime(t *testing.T) {
	s := newTestSynth(t, 2)

	iset := s.GetInstruments()
	if iset.numChannels != 2 {
		t.Fatalf("instruments channel count mismatch: %d", iset.numChannels)
	}
	if len(iset.instOnly.noteTab) != 0 || len(iset.instOnly.effectTab) != 0 {
		t.Fatalf("GetInstruments leaked per-trial tables: notes=%d effects=%d",
			len(iset.instOnly.noteTab), len(iset.instOnly.effectTab))
	}

	if err := s.PlayNote(0.05, newInstrumentNote(53, 1), newInstrumentNote(60, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	renderSynthTicks(t, s, 4)

	if err := s.SetInstruments(iset); err != nil {
		t.Fatalf("set instruments: %v", err)
	}
	if len(s.stream.activeChannels) != 0 {
		t.Fatalf("SetInstruments left active channels: %d", len(s.stream.activeChannels))
	}
	if s.stream.globalVolume != 1.0 {
		t.Fatalf("SetInstruments left global volume: %v", s.stream.globalVolume)
	}

	// Instruments restored via SetInstruments must be reusable.
	if err := s.PlayNote(0.05, newInstrumentNote(53, 1), newInstrumentNote(60, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	renderSynthTicks(t, s, 4)
}

// TestSynthLoadInstrumentsFailureKeepsPrevious checks that a failed
// LoadInstruments leaves the previous instruments usable.
func TestSynthLoadInstrumentsFailureKeepsPrevious(t *testing.T) {
	s := newTestSynth(t, 2)

	if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	ref := renderSynthTicks(t, s, 4)

	// A module with an Amiga frequency table can't be compiled.
	failing := newTestModuleBuilder(2)
	failing.addInstrument(makeTestInstrument(120, 64, 128, 4000))
	failing.addPattern([]testNoteSpec{{note: 1 + 53, instrument: 1}})
	failingModule := failing.build()
	failingModule.Flags = 0

	if err := s.LoadInstruments(failingModule, LoadModuleConfig{}); err == nil {
		t.Fatalf("expected a load error")
	}

	// The old instruments are still loaded and produce the same output.
	if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
		t.Fatalf("play note after failed load: %v", err)
	}
	got := renderSynthTicks(t, s, 4)
	assertBytesEqual(t, "playback after failed LoadInstruments", got, ref)
}

// TestSynthTempoDoesNotLeakIntoModule checks that the trial tempo
// doesn't modify the shared compiled module tempo.
func TestSynthTempoDoesNotLeakIntoModule(t *testing.T) {
	s := newTestSynth(t, 2)
	bpm := s.stream.module.bpm
	tempo := s.stream.module.ticksPerRow

	if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	if s.stream.module.ticksPerRow != tempo {
		t.Fatalf("trial changed the module tempo: got %d, want %d",
			s.stream.module.ticksPerRow, tempo)
	}
	if s.stream.module.bpm != bpm {
		t.Fatalf("trial changed the module bpm: got %v, want %v",
			s.stream.module.bpm, bpm)
	}
	renderSynthTicks(t, s, 4)
}

// TestSynthConsecutivePlayNoteWithoutRewind checks a realistic use case:
// a game plays trial notes back to back without explicitly rewinding
// between them. Every PlayNote must start from a clean state on its own.
func TestSynthConsecutivePlayNoteWithoutRewind(t *testing.T) {
	ref := func() []byte {
		s := newTestSynth(t, 1)
		if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
			t.Fatalf("play note: %v", err)
		}
		return renderSynthWholeTrial(t, s)
	}()

	s := newTestSynth(t, 1)
	for _, inst := range []uint8{1, 2, 1, 2, 1} {
		if err := s.PlayNote(0.05, newInstrumentNote(53, inst)); err != nil {
			t.Fatalf("play note: %v", err)
		}
		// Drain the trial fully so the stream reaches EOF.
		renderSynthWholeTrial(t, s)
		if inst == 1 {
			// After the full trial we're at EOF; PlayNote must recover
			// from that without an external Rewind.
			got := func() []byte {
				if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
					t.Fatalf("play note: %v", err)
				}
				return renderSynthWholeTrial(t, s)
			}()
			assertBytesEqual(t, "consecutive trial", got, ref)
		}
	}
}

// TestSynthSharedInstrumentsAreIsolated checks that two synthesizers
// sharing the same collected instruments don't leak trial state into
// each other.
func TestSynthSharedInstrumentsAreIsolated(t *testing.T) {
	a := newTestSynth(t, 2)
	iset := a.GetInstruments()

	b := NewSynthesizer(SynthesizerConfig{NumChannels: 2})
	if err := b.SetInstruments(iset); err != nil {
		t.Fatalf("set instruments: %v", err)
	}

	// Prepare a trial on A.
	if err := a.PlayNote(0.05, newInstrumentNote(53, 1), newInstrumentNote(60, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	renderSynthTicks(t, a, 4)

	// B must still start from a clean state.
	if err := b.PlayNote(0.05, newInstrumentNote(53, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	if len(b.stream.module.noteTab) != 2 {
		t.Fatalf("B note tab size mismatch: %d", len(b.stream.module.noteTab))
	}
	if b.stream.module.noteTab[0].inst == nil {
		t.Fatalf("B channel 0 has no instrument")
	}
	if b.stream.module.noteTab[0].inst.id != 1 {
		t.Fatalf("B channel 0 has the wrong instrument: got id %d, want 1",
			b.stream.module.noteTab[0].inst.id)
	}
	// B's output must match a fresh synthesizer playing the same note,
	// so A's trial state (and A's tempo) must not have leaked into B.
	ref := newTestSynth(t, 2)
	if err := ref.PlayNote(0.05, newInstrumentNote(53, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	want := renderSynthWholeTrial(t, ref)
	got := renderSynthWholeTrial(t, b)
	assertBytesEqual(t, "shared instruments isolation", got, want)
}

// TestSynthPlayNoteClearsChannelRuntimeState checks that PlayNote wipes
// the per-channel runtime state left by a previous trial.
func TestSynthPlayNoteClearsChannelRuntimeState(t *testing.T) {
	s := newTestSynth(t, 2)
	if err := s.PlayNote(0.05, newInstrumentNote(53, 1), newInstrumentNote(60, 2)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	renderSynthTicks(t, s, 4)

	// Now dirty every channel's runtime state, simulating a long trial.
	for i := range s.stream.channels {
		ch := &s.stream.channels[i]
		ch.volume = 0.2
		ch.panning = 0.95
		ch.fadeoutVolume = 0.4
		ch.period = 2000
		ch.vibratoRunning = true
		ch.vibratoPeriodOffset = 9
		ch.arpeggioRunning = true
		ch.arpeggioNoteOffset = 2
		ch.volumeSlideValue = 0.3
		ch.globalVolumeSlideValue = 0.3
		ch.panningSlideValue = 0.3
		ch.notePortamentoValue = 7
		ch.notePortamentoTargetPeriod = 1500
		ch.keyOn = true
		ch.volumeEnvelope.frame = 5
		ch.panningEnvelope.frame = 4
		ch.vibratoStep = 6
		ch.vibratoSpeed = 3
		ch.vibratoDepth = 5
	}
	s.stream.globalVolume = 0.2

	// A new PlayNote must reset all of that before assigning new notes.
	if err := s.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
		t.Fatalf("play note: %v", err)
	}

	// Global volume goes back to its default before the first tick.
	if s.stream.globalVolume != 1.0 {
		t.Fatalf("global volume was not reset: %v", s.stream.globalVolume)
	}
	if s.stream.bytePos != 0 {
		t.Fatalf("byte position was not reset: %d", s.stream.bytePos)
	}

	// The unused channel must be completely clean.
	unused := &s.stream.channels[1]
	if !isChannelClean(unused) {
		t.Fatalf("unused channel still has stale runtime state: %+v", *unused)
	}

	// The used channel must be re-initialized from the new note: the
	// stale vibrato/arpeggio/slide values must be gone.
	used := &s.stream.channels[0]
	if used.vibratoRunning || used.vibratoPeriodOffset != 0 {
		t.Fatalf("used channel kept stale vibrato state: %+v", *used)
	}
	if used.arpeggioRunning || used.arpeggioNoteOffset != 0 {
		t.Fatalf("used channel kept stale arpeggio state: %+v", *used)
	}
	if used.volumeSlideValue != 0 || used.globalVolumeSlideValue != 0 || used.panningSlideValue != 0 {
		t.Fatalf("used channel kept stale slide values: %+v", *used)
	}
	if used.notePortamentoValue != 0 || used.notePortamentoTargetPeriod != 0 {
		t.Fatalf("used channel kept stale portamento state: %+v", *used)
	}
	if used.vibratoStep != 0 || used.vibratoSpeed != 0 || used.vibratoDepth != 0 {
		t.Fatalf("used channel kept stale vibrato params: %+v", *used)
	}

	// The output must match a fresh synthesizer playing the same note.
	ref := newTestSynth(t, 2)
	if err := ref.PlayNote(0.05, newInstrumentNote(53, 1)); err != nil {
		t.Fatalf("play note: %v", err)
	}
	want := renderSynthTicks(t, ref, 4)
	got := renderSynthTicks(t, s, 4)
	assertBytesEqual(t, "trial after dirty state", got, want)
}
