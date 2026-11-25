package xm

import (
	"io"
	"testing"

	"github.com/quasilyte/xm/xmfile"
)

// These tests cover the stream module reload behavior.
//
// They don't use any real audio device: the stream is only advanced
// via Read() with in-memory byte slices.

func testModuleA() *xmfile.Module {
	b := newTestModuleBuilder(2)
	inst := b.addInstrument(makeTestInstrument(100, 64, 128, 64))
	b.addPattern(
		[]testNoteSpec{{note: 1 + 48, instrument: uint8(inst)}},
		[]testNoteSpec{{note: 1 + 52, instrument: uint8(inst)}},
		[]testNoteSpec{{note: 1 + 55, instrument: uint8(inst)}},
		[]testNoteSpec{{note: 1 + 58, instrument: uint8(inst)}},
	)
	return b.build()
}

func testModuleB() *xmfile.Module {
	b := newTestModuleBuilder(3)
	inst := b.addInstrument(makeTestInstrument(60, 32, 64, 32))
	b.addPattern(
		[]testNoteSpec{{note: 1 + 60, instrument: uint8(inst)}},
		[]testNoteSpec{{note: 1 + 64, instrument: uint8(inst)}},
	)
	return b.build()
}

// TestStreamReloadResetsState checks that loading a second module into
// the same stream discards all of the previous module's runtime state.
func TestStreamReloadResetsState(t *testing.T) {
	s := mustLoadModule(t, testModuleA(), LoadModuleConfig{})

	// Advance the first module for a while so that the channels really
	// accumulate some runtime state (volume/panning/envelopes/etc).
	renderTicks(t, s, 8)

	// Position after the first run.
	pos, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if pos == 0 {
		t.Fatalf("expected a non-zero byte position after reading")
	}

	// Reload a different module. This must reset the position and state.
	if err := s.LoadModule(testModuleB(), LoadModuleConfig{}); err != nil {
		t.Fatalf("reload: %v", err)
	}

	pos, err = s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if pos != 0 {
		t.Fatalf("byte position was not reset after reload: got %d, want 0", pos)
	}

	// The output of a freshly reloaded module must match the output of
	// the very same module played by a brand new stream.
	fresh := mustLoadModule(t, testModuleB(), LoadModuleConfig{})
	want := renderTicks(t, fresh, 6)
	got := renderTicks(t, s, 6)
	assertBytesEqual(t, "reloaded module output", got, want)
}

// TestStreamReloadTwice checks that repeated reloads keep producing
// the same output as a freshly created stream (no cumulative drift).
func TestStreamReloadTwice(t *testing.T) {
	s := NewStream()
	if err := s.LoadModule(testModuleA(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load A: %v", err)
	}
	renderTicks(t, s, 4)

	modB := testModuleB()
	for i := 0; i < 3; i++ {
		if err := s.LoadModule(modB, LoadModuleConfig{}); err != nil {
			t.Fatalf("iteration %d: reload B: %v", i, err)
		}
		got := renderTicks(t, s, 6)

		fresh := mustLoadModule(t, modB, LoadModuleConfig{})
		want := renderTicks(t, fresh, 6)
		assertBytesEqual(t, "reload iteration", got, want)
	}
}

// TestStreamLoadFailureKeepsOldModule checks that a failed LoadModule
// leaves the previously loaded module fully playable.
func TestStreamLoadFailureKeepsOldModule(t *testing.T) {
	modA := testModuleA()
	s := mustLoadModule(t, modA, LoadModuleConfig{})

	// Record what the next 6 ticks of module A would sound like.
	ref := mustLoadModule(t, modA, LoadModuleConfig{})
	want := renderTicks(t, ref, 6)

	// Register a broken module: an unsupported sample rate.
	// The stream must report the error and must not change its state.
	if err := s.LoadModule(testModuleB(), LoadModuleConfig{SampleRate: 22050}); err == nil {
		t.Fatalf("expected an error for an unsupported sample rate")
	}

	pos, _ := s.Seek(0, io.SeekCurrent)
	if pos != 0 {
		t.Fatalf("failed load changed the byte position: got %d, want 0", pos)
	}

	got := renderTicks(t, s, 6)
	assertBytesEqual(t, "playback after failed load", got, want)
}

// TestStreamLoadFailureKeepsPlaybackPosition checks that a failed load
// after some playback doesn't rewind or otherwise disturb the stream.
func TestStreamLoadFailureKeepsPlaybackPosition(t *testing.T) {
	modA := testModuleA()
	s := mustLoadModule(t, modA, LoadModuleConfig{})

	// A compilation error (not a config error) must not disturb the stream.
	broken := newTestModuleBuilder(2)
	broken.addInstrument(makeTestInstrument(100, 64, 128, 64))
	broken.addPattern([]testNoteSpec{{note: 1 + 48, instrument: 1}})
	brokenModule := broken.build()
	brokenModule.Flags = 0 // The Amiga frequency table is not supported.

	// Play a bit of module A first; remember the following output.
	renderTicks(t, s, 2)
	posBefore, _ := s.Seek(0, io.SeekCurrent)

	want := renderTicks(t, mustLoadModule(t, modA, LoadModuleConfig{}), 8)[2*int(s.GetInfo().BytesPerTick):]

	if err := s.LoadModule(brokenModule, LoadModuleConfig{}); err == nil {
		t.Fatalf("expected a compilation error")
	}

	posAfter, _ := s.Seek(0, io.SeekCurrent)
	if posAfter != posBefore {
		t.Fatalf("failed load changed the playback position: got %d, want %d", posAfter, posBefore)
	}

	got := renderTicks(t, s, 6)
	assertBytesEqual(t, "playback continues after failed compile", got, want[:len(got)])
}

// TestStreamLoadFailureKeepsChannelLayout checks that a failed load of a
// module with a different channel count doesn't leave the stream in a
// half-updated state: the old module must still play with its old
// channel count.
func TestStreamLoadFailureKeepsChannelLayout(t *testing.T) {
	modA := testModuleA() // 2 channels
	s := mustLoadModule(t, modA, LoadModuleConfig{})

	// A 4-channel module with an Amiga frequency table fails to compile.
	broken := newTestModuleBuilder(4)
	broken.addInstrument(makeTestInstrument(100, 64, 128, 64))
	broken.addPattern([]testNoteSpec{{note: 1 + 48, instrument: 1}})
	brokenModule := broken.build()
	brokenModule.Flags = 0

	if err := s.LoadModule(brokenModule, LoadModuleConfig{}); err == nil {
		t.Fatalf("expected a compilation error")
	}

	// The old 2-channel module must still be there, untouched.
	if len(s.channels) != 2 {
		t.Fatalf("failed load changed the channel count: got %d, want 2", len(s.channels))
	}
	want := renderTicks(t, mustLoadModule(t, modA, LoadModuleConfig{}), 6)
	got := renderTicks(t, s, 6)
	assertBytesEqual(t, "playback after failed load of a bigger module", got, want)
}

// TestStreamRewindResetsPosition checks that Rewind resets the seek
// position and that playback is identical to the first run.
func TestStreamRewindResetsPosition(t *testing.T) {
	s := mustLoadModule(t, testModuleA(), LoadModuleConfig{})

	first := renderTicks(t, s, 4)
	if _, err := s.Seek(0, io.SeekEnd); err == nil {
		t.Fatalf("SeekEnd must be rejected")
	}

	s.Rewind()
	pos, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if pos != 0 {
		t.Fatalf("rewind did not reset the byte position: got %d", pos)
	}

	second := renderTicks(t, s, 4)
	assertBytesEqual(t, "playback after rewind", second, first)
}

// TestStreamEventsAfterReload checks that event callbacks don't leak
// the previous module's time to the caller after a reload or rewind.
func TestStreamEventsAfterReload(t *testing.T) {
	s := mustLoadModule(t, testModuleA(), LoadModuleConfig{})

	var events []StreamEvent
	s.SetEventHandler(func(e StreamEvent) {
		events = append(events, e)
	})

	renderTicks(t, s, 4)
	if len(events) == 0 {
		t.Fatalf("expected note events during playback")
	}
	for _, e := range events {
		if e.Time < 0 {
			t.Fatalf("unexpected negative event time: %v", e.Time)
		}
	}

	// Reloading must emit a sync event at time 0.
	events = events[:0]
	if err := s.LoadModule(testModuleB(), LoadModuleConfig{}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event after reload, got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != EventSync {
		t.Fatalf("expected a sync event after reload, got kind %v", ev.Kind)
	}
	if ev.Time != 0 {
		t.Fatalf("sync event after reload has stale time: got %v, want 0", ev.Time)
	}
	if got := ev.SyncEventData(); got != 0 {
		t.Fatalf("sync event after reload has stale data: got %v, want 0", got)
	}

	// Playing the new module should produce events starting from time 0.
	events = events[:0]
	renderTicks(t, s, 4)
	if len(events) == 0 {
		t.Fatalf("expected note events after reload")
	}
	if events[0].Time == 0 && len(events) > 1 {
		// Events are ordered by time; the first note should be at t>0.
	}
	for i := 1; i < len(events); i++ {
		if events[i].Time < events[i-1].Time {
			t.Fatalf("event times are not monotonic: %v then %v", events[i-1].Time, events[i].Time)
		}
	}

	// Rewind must also emit a sync event at time 0, not the old time.
	events = events[:0]
	s.Rewind()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event after rewind, got %d", len(events))
	}
	if events[0].Kind != EventSync || events[0].Time != 0 {
		t.Fatalf("rewind sync event is stale: kind=%v time=%v", events[0].Kind, events[0].Time)
	}
}

// TestStreamChannelsResetAfterReload verifies that the per-channel
// runtime state is wiped when a module is reloaded.
func TestStreamChannelsResetAfterReload(t *testing.T) {
	s := mustLoadModule(t, testModuleA(), LoadModuleConfig{})

	// Dirty the channel state directly to simulate a long playback.
	if err := touchChannelState(s); err != nil {
		t.Fatalf("touch state: %v", err)
	}

	if err := s.LoadModule(testModuleB(), LoadModuleConfig{}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(s.channels) != 3 {
		t.Fatalf("channel count mismatch: got %d, want 3", len(s.channels))
	}
	for i := range s.channels {
		ch := &s.channels[i]
		if !isChannelClean(ch) {
			t.Fatalf("channel %d was not reset after reload: %+v", i, *ch)
		}
		if ch.id != i {
			t.Fatalf("channel %d has wrong id: %d", i, ch.id)
		}
	}
	if len(s.activeChannels) != 0 {
		t.Fatalf("active channels were not reset: %d", len(s.activeChannels))
	}
	if s.globalVolume != 1.0 {
		t.Fatalf("global volume was not reset: %v", s.globalVolume)
	}
	if s.bytePos != 0 {
		t.Fatalf("byte position was not reset: %d", s.bytePos)
	}
}

// testModuleWithTimingEffects builds a module whose first rows change the
// playback timing and the global volume via effects.
func testModuleWithTimingEffects() *xmfile.Module {
	b := newTestModuleBuilder(2)
	inst := b.addInstrument(makeTestInstrument(100, 64, 128, 64))
	b.addPattern(
		// Fxx: set BPM to 200 (and set the global volume to 0.5).
		[]testNoteSpec{
			{note: 1 + 48, instrument: uint8(inst), effect: 0x0F, param: 200},
			{effect: 0x10, param: 32},
		},
		// Fxx: set tempo (spd) to 3.
		[]testNoteSpec{
			{effect: 0x0F, param: 3},
		},
		[]testNoteSpec{{note: 1 + 52, instrument: uint8(inst)}},
		[]testNoteSpec{{note: 1 + 55, instrument: uint8(inst)}},
	)
	return b.build()
}

// TestStreamReloadResetsTimingAndVolume checks that effects applied during
// playback (BPM, tempo, global volume) are not carried into a reload.
func TestStreamReloadResetsTimingAndVolume(t *testing.T) {
	mod := testModuleWithTimingEffects()
	s := mustLoadModule(t, mod, LoadModuleConfig{})

	// Play the rows that change BPM, tempo and global volume.
	renderUntilEOF(t, s)

	if s.bpm == float64(mod.DefaultBPM) {
		t.Fatalf("test setup is wrong: the BPM effect had no effect")
	}
	if s.ticksPerRow == mod.DefaultTempo {
		t.Fatalf("test setup is wrong: the tempo effect had no effect")
	}
	if s.globalVolume == 1.0 {
		t.Fatalf("test setup is wrong: the global volume effect had no effect")
	}

	// Reload the same module. All of the values must return to defaults.
	if err := s.LoadModule(mod, LoadModuleConfig{}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s.bpm != float64(mod.DefaultBPM) {
		t.Fatalf("BPM was not reset after reload: got %v, want %v",
			s.bpm, float64(mod.DefaultBPM))
	}
	if s.ticksPerRow != mod.DefaultTempo {
		t.Fatalf("tempo was not reset after reload: got %d, want %d",
			s.ticksPerRow, mod.DefaultTempo)
	}
	if s.globalVolume != 1.0 {
		t.Fatalf("global volume was not reset after reload: got %v, want 1", s.globalVolume)
	}
	if s.bytePos != 0 {
		t.Fatalf("byte position was not reset after reload: got %d", s.bytePos)
	}

	// And the rendered output matches a fresh stream playing this module.
	fresh := mustLoadModule(t, mod, LoadModuleConfig{})
	want := renderUntilEOF(t, fresh)
	got := renderUntilEOF(t, s)
	assertBytesEqual(t, "reload reset timing/volume", got, want)
}

func touchChannelState(s *Stream) error {
	for i := range s.channels {
		ch := &s.channels[i]
		ch.volume = 0.25
		ch.panning = 0.9
		ch.fadeoutVolume = 0.5
		ch.period = 1234
		ch.vibratoRunning = true
		ch.vibratoPeriodOffset = 12
		ch.arpeggioRunning = true
		ch.arpeggioNoteOffset = 3
		ch.volumeSlideValue = 0.1
		ch.notePortamentoValue = 8
		ch.notePortamentoTargetPeriod = 999
		ch.keyOn = true
		ch.volumeEnvelope.frame = 7
		ch.panningEnvelope.frame = 3
	}
	s.globalVolume = 0.1
	s.activeChannels = append(s.activeChannels, &s.channels[0])
	return nil
}

func isChannelClean(ch *streamChannel) bool {
	return ch.inst == nil &&
		ch.volume == 0 &&
		ch.panning == 0 &&
		ch.fadeoutVolume == 0 &&
		ch.period == 0 &&
		!ch.vibratoRunning &&
		ch.vibratoPeriodOffset == 0 &&
		!ch.arpeggioRunning &&
		ch.arpeggioNoteOffset == 0 &&
		ch.volumeSlideValue == 0 &&
		ch.notePortamentoValue == 0 &&
		ch.notePortamentoTargetPeriod == 0 &&
		!ch.keyOn &&
		ch.volumeEnvelope.frame == 0 &&
		ch.panningEnvelope.frame == 0
}
