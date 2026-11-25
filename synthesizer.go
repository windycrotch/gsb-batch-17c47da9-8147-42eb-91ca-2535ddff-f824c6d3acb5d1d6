package xm

import (
	"errors"
	"io"

	"github.com/quasilyte/xm/xmfile"
)

// Synthesizer can be used to play individual XM notes.
//
// It is more efficient and convenient to use for this
// use case than a stream with a constant module re-loading.
//
// Experimental: synthesizer API may change in the near future.
type Synthesizer struct {
	stream       *Stream
	noteCompiler *moduleCompiler

	// trialTempo is the number of ticks per row for the current trial.
	// It's re-applied after a rewind, since the stream otherwise falls
	// back to the module default tempo.
	// A zero value means "no trial is prepared".
	trialTempo int
}

type SynthesizerInstruments struct {
	instOnly    module
	numChannels int
}

// GetInstrumentMapping returns a mapping of an actual compiled
// instrument id to the index from the source XM module.
// They can be identical and the mapping would look like x=>x,
// but if some instruments were invalid or empty, the mapping
// will not be 1-to-1.
func (iset *SynthesizerInstruments) GetInstrumentMapping() []int {
	num := 0
	for _, inst := range iset.instOnly.instruments {
		if inst.id != -1 {
			num++
		}
	}
	m := make([]int, 0, num)
	for _, inst := range iset.instOnly.instruments {
		if inst.id != -1 {
			m = append(m, inst.id)
		}
	}
	return m
}

type SynthesizerConfig struct {
	NumChannels int
}

func NewSynthesizer(config SynthesizerConfig) *Synthesizer {
	stream := NewStream()
	stream.channels = make([]streamChannel, config.NumChannels)
	stream.activeChannels = make([]*streamChannel, config.NumChannels)
	return &Synthesizer{
		stream:       stream,
		noteCompiler: newSynthCompiler(),
	}
}

// SetVolume adjusts the global volume scaling for the underlying stream.
func (s *Synthesizer) SetVolume(v float64) {
	s.stream.SetVolume(v)
}

// SetInstruments loads the instruments from a list received from GetInstruments.
func (s *Synthesizer) SetInstruments(is *SynthesizerInstruments) error {
	if len(s.stream.channels) != is.numChannels {
		return errors.New("mismatching channel count")
	}

	s.stream.assignCompiledModule(is.instOnly)

	return nil
}

// GetInstruments returns the previously loaded instruments.
// It is intended to be used in combination with SetInstruments
// to avoid the need for repeated LoadInstruments for the
// same module. It is much more efficient as it avoids
// the module re-compilation.
// The instruments are only compatible if the number of channels is matching.
func (s *Synthesizer) GetInstruments() *SynthesizerInstruments {
	instOnly := s.stream.module
	// Don't share the per-play trial data (effect and note tables)
	// with the snapshot; they're overwritten by every PlayNote call.
	instOnly.effectTab = nil
	instOnly.noteTab = nil
	return &SynthesizerInstruments{
		instOnly:    instOnly,
		numChannels: len(s.stream.channels),
	}
}

// LoadInstruments prepares the instruments from the module
// for further use.
//
// Loading instruments involves module compilation,
// so it should not be called on a hot path repeatedly.
// If you need multiple modules at once, consider
// using several synthesizers, one per module.
//
// The patterns don't really matter as this method
// is only interested in instruments (and samples).
func (s *Synthesizer) LoadInstruments(m *xmfile.Module, config LoadModuleConfig) error {
	s.stream.applyConfigDefaults(m, &config)

	instOnly, err := s.compileModuleInstruments(m, config)
	if err != nil {
		return err
	}

	s.stream.assignCompiledModule(instOnly)

	return nil
}

func (s *Synthesizer) compileModuleInstruments(m *xmfile.Module, config LoadModuleConfig) (module, error) {
	instOnly := xmfile.Module{
		Name:           m.Name,
		Version:        m.Version,
		NumInstruments: m.NumInstruments,
		Flags:          m.Flags,
		Instruments:    m.Instruments,
	}

	compiled, err := compileModule(&instOnly, moduleConfig{
		sampleRate: config.SampleRate,
		bpm:        config.BPM,
		tempo:      config.Tempo,
		subSamples: config.LinearInterpolation,
	})
	if err != nil {
		return compiled, err
	}

	compiled.patterns = []pattern{
		{
			numChannels: len(s.stream.channels),
			numRows:     1,
			notes:       make([]uint16, len(s.stream.channels)),
		},
	}
	compiled.patternOrder = []*pattern{
		&compiled.patterns[0],
	}

	pat := &compiled.patterns[0]
	for i := range pat.notes {
		pat.notes[i] = uint16(i)
	}

	return compiled, nil
}

func (s *Synthesizer) prepareToPlay(duration float64) {
	// Start every trial from a clean state. rewind() drops the channel
	// runtime state (volumes, panning, envelopes, sample positions,
	// vibrato/arpeggio/portamento/effect values), the stream position,
	// the global volume and the timing values.
	s.stream.rewind()

	// Clear the per-trial tables as well. The effect table must be
	// rebuilt from scratch, since the effect keys of the compiled
	// notes index into it.
	s.noteCompiler.reset(&s.stream.module)
	s.stream.module.effectTab = s.stream.module.effectTab[:0]
	s.stream.module.noteTab = s.stream.module.noteTab[:0]

	if duration == 0 {
		s.trialTempo = 240
	} else {
		s.trialTempo = 1 + int(ticksPerSecond(s.stream.bpm)*duration)
	}
	s.stream.setTempo(s.trialTempo)
}

// rewindTrialRewind rewinds the underlying stream and re-applies the
// current trial tempo, so that a single trial can be replayed with the
// same timing as when it was prepared by PlayNote.
func (s *Synthesizer) rewindTrial() {
	s.stream.Rewind()
	if s.trialTempo != 0 {
		s.stream.setTempo(s.trialTempo)
	}
}

// PlayNote plays one or more notes up to the specified duration.
// Using 0 for the duration will play it for several seconds.
//
// Every note is assigned to its own channel, in the order they were
// passed. Notes that don't fit into the configured number of channels
// are ignored. Any trial state left by the previous PlayNote call is
// discarded before the new notes are prepared.
func (s *Synthesizer) PlayNote(duration float64, notes ...xmfile.PatternNote) error {
	s.prepareToPlay(duration)

	numChannels := len(s.stream.channels)
	chansUsed := 0
	for _, note := range notes {
		if chansUsed >= numChannels {
			// Too many notes: the remaining ones are ignored.
			break
		}
		n, _, err := s.noteCompiler.compileNote(note)
		if err != nil {
			// Keep the stream in a consistent silent state instead of
			// leaving a half-compiled effect table behind.
			s.clearTrialNotes()
			return err
		}
		s.stream.module.noteTab = append(s.stream.module.noteTab, n)
		chansUsed++
	}

	// Every channel must have a note entry, even if there is no note
	// for it; otherwise the pattern data would index out of bounds.
	for chansUsed < numChannels {
		s.stream.module.noteTab = append(s.stream.module.noteTab, patternNote{})
		chansUsed++
	}

	return nil
}

// clearTrialNotes replaces the trial notes with silent ones so the
// stream can be read safely after a failed PlayNote call.
func (s *Synthesizer) clearTrialNotes() {
	s.stream.module.effectTab = s.stream.module.effectTab[:0]
	s.stream.module.noteTab = s.stream.module.noteTab[:0]
	for i := 0; i < len(s.stream.channels); i++ {
		s.stream.module.noteTab = append(s.stream.module.noteTab, patternNote{})
	}
}

func (s *Synthesizer) Read(b []byte) (int, error) {
	return s.stream.Read(b)
}

func (s *Synthesizer) Rewind() {
	s.rewindTrial()
}

func (s *Synthesizer) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekStart && offset == 0 {
		s.rewindTrial()
		return 0, nil
	}
	if whence == io.SeekCurrent && offset == 0 {
		return int64(s.stream.bytePos), nil
	}
	return 0, errors.New("unsupported Seek call")
}
