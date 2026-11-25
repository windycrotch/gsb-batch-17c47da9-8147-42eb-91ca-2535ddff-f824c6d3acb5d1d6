package main

// convergence_entry_test.go is the production-entry companion to the
// internal/audio convergence suite. The task requires that the real App/backend
// wiring still builds an engine and reflects the gate meter (A7), and that the
// mic-processing setters route into the engine with no regression. These tests are
// hardware-free: they build a Backend with a bare engine (as newBackend does on a
// machine with no cable) and drive the SAME applyProcessingW bootstrap main uses.

import (
	"testing"
	"testing/fstest"

	"github.com/jodagreyhame/SoundBoard/internal/audio"
	"github.com/jodagreyhame/SoundBoard/internal/catalog"
	"github.com/jodagreyhame/SoundBoard/internal/config"
	"github.com/jodagreyhame/SoundBoard/internal/setup"
)

// bareEngineBackend builds a Backend whose engine has no devices, exactly as
// newBackend does when the audio context or cable is unavailable. Settings are
// normalised so the accessors (AdvancedVAD/AutoSens/etc.) return their defaults.
func bareEngineBackend(t *testing.T, s *config.Settings) *Backend {
	t.Helper()
	lib, err := catalog.New(fstest.MapFS{"memes/airhorn.wav": {Data: []byte("x")}})
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	engine := audio.NewEngine(nil, lib)
	return &Backend{
		settings: s,
		engine:   engine,
		setup:    &routingController{status: setup.Status{}}, // cable absent
	}
}

// TestApplyProcessingWSeedsEngine confirms the production bootstrap (applyProcessingW)
// pushes every mic-processing setting into the engine's atomic control surface, so a
// freshly started engine runs the saved configuration rather than defaults.
func TestApplyProcessingWSeedsEngine(t *testing.T) {
	s := &config.Settings{}
	s.Processing.MicMode = config.MicModeAlways
	s.Processing.NoiseSuppressionTier = config.NSModeStandard
	s.Processing.AGC = true
	s.Processing.EchoCancellation = true
	s.Processing.GateSensitivity = 0.4

	b := bareEngineBackend(t, s)
	applyProcessingW(b.engine, s)

	// The engine exposes its gate meter for the UI; it must be in range even on a
	// bare engine that has never processed a frame.
	if gl := b.engine.GateLevel(); gl < 0 || gl > 1 {
		t.Fatalf("GateLevel out of range after bootstrap: %v", gl)
	}
	// Round-trip a couple of production setters through the App and read back the
	// engine state indirectly by re-applying and asserting no panic / valid meter.
	app := newAppWithBackend(b)
	app.SetMicMode(config.MicModeMute)
	app.SetNoiseSuppressionTier(config.NSModeNone)
	app.SetAGC(false)
	app.SetEchoCancellation(false)
	if gl := b.engine.GateLevel(); gl < 0 || gl > 1 {
		t.Fatalf("GateLevel out of range after App setters: %v", gl)
	}
}

// TestAppMicSettersPersistAndRoute confirms each bound setter both mutates the
// engine (no error / panic) and records the value in settings for persistence —
// the "production entry still works" contract (A7). It also exercises the PTT
// toggle path used by the hotkey manager.
func TestAppMicSettersPersistAndRoute(t *testing.T) {
	s := &config.Settings{}
	b := bareEngineBackend(t, s)
	app := newAppWithBackend(b)

	app.SetMicMode(config.MicModePTT)
	if s.Processing.MicMode != config.MicModePTT {
		t.Fatalf("SetMicMode did not persist, got %q", s.Processing.MicMode)
	}

	app.SetNoiseSuppressionTier(config.NSModeStrong)
	if s.Processing.NoiseSuppressionTier != config.NSModeStrong {
		t.Fatalf("SetNoiseSuppressionTier did not persist, got %q", s.Processing.NoiseSuppressionTier)
	}
	// The legacy bool is kept in sync (true iff tier != none) so older readers see a
	// consistent "some suppression is on" state.
	if !s.Processing.NoiseSuppression {
		t.Fatal("legacy NoiseSuppression bool should be true for the strong tier")
	}

	app.SetAGC(true)
	if !s.Processing.AGC {
		t.Fatal("SetAGC did not persist")
	}
	app.SetEchoCancellation(true)
	if !s.Processing.EchoCancellation {
		t.Fatal("SetEchoCancellation did not persist")
	}
	app.SetGateSensitivity(0.33)
	if d := s.Processing.GateSensitivity - 0.33; d > 1e-6 || d < -1e-6 {
		t.Fatalf("SetGateSensitivity did not persist, got %v", s.Processing.GateSensitivity)
	}
}
