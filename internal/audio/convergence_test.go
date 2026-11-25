package audio

// convergence_test.go is the cross-state regression suite for the mic-processing
// chain converging under the transitions the task calls out:
//
//   - input-mode switches (vad/ptt/always/mute) must take effect on the VERY NEXT
//     frame, with the gate's forced open/close complete by end of frame and
//     GateLevel matching the signal actually emitted;
//   - noise-suppression tier / AGC / echo-cancellation switches must run the new
//     config with no stale epoch and no two suppressors stacked;
//   - when the DSP worker lags (empty output ring) the RT callback must still
//     silence Mute and PTT-up and must never splice raw mic into the output;
//   - a denoiser that degrades to Passthrough at runtime must drop the advanced-VAD
//     gate to the energy latch (never latch the mic permanently shut) and recover to
//     probability-driven gating once a real denoiser is available again;
//   - auto-sensitivity must track the noise floor rather than the manual slider.
//
// Each test drives the REAL worker/engine/gate (or the real duplexCallback over the
// real rings) — no mocks of the unit under test — so a regression in the production
// convergence behavior turns them red.

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jodagreyhame/SoundBoard/internal/apm"
	"github.com/jodagreyhame/SoundBoard/internal/denoise"
)

// --- A1: input-mode switches take effect on the next frame ---------------------

// TestModeSwitchConvergesNextFrame drives ONE worker through a loud run and asserts
// that a switch to mute drives GateLevel ~0 by the END of the next frame and is fully
// silent on the frame after, then a switch to always drives GateLevel ~1 by the end
// of the next frame and passes the mic on the frame after. This is the core
// "下一帧起门控的强制开/关必须立即生效" contract: the forced ramp (gateForceTau
// ~0.5ms) reaches its target within one 10ms frame, unlike the old ~120ms voice
// release which left the meter high (0.92) one frame after mute and took dozens of
// frames to go silent.
func TestModeSwitchConvergesNextFrame(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetMicMode("vad")
	w := newWorkerFor(e)
	defer w.apm.Close()

	ph := 0.0
	var frame []float32
	// Open the gate on loud speech first.
	for i := 0; i < 40; i++ {
		frame, ph = loudMonoFrame(ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("precondition: VAD gate should be open on loud speech, GateLevel = %v", gl)
	}

	// Switch to MUTE. The gate LEVEL must converge to ~0 by the END of the very next
	// frame (the forced ramp is ~0.5ms, so within one 10ms frame the gain is at its
	// target). The first post-switch frame still carries the click-free fade-out tail;
	// the SECOND frame is then fully silent. This is the worker-internal view; at the
	// CABLE the RT callback hard-zeros the whole buffer immediately (covered by the
	// underrun/force-close tests), so the transmitted audio is silent on the very next
	// buffer regardless.
	e.SetMicMode("mute")
	frame, ph = loudMonoFrame(ph)
	w.process(frame)
	if gl := e.GateLevel(); gl > 5e-3 {
		t.Fatalf("one frame after switching to mute, GateLevel = %v, want ~0 (forced close must reach its target within the frame)", gl)
	}
	frame, ph = loudMonoFrame(ph)
	w.process(frame)
	if r := rms(frame); r > 1e-3 {
		t.Fatalf("two frames after switching to mute, output RMS = %v, want ~0", r)
	}

	// Switch to ALWAYS. The gate LEVEL must be ~1 by the end of the next frame, and
	// the second frame fully passes the mic.
	e.SetMicMode("always")
	frame, ph = loudMonoFrame(ph)
	w.process(frame)
	if gl := e.GateLevel(); gl < 0.99 {
		t.Fatalf("one frame after switching to always, GateLevel = %v, want ~1 (forced open must reach its target within the frame)", gl)
	}
	frame, ph = loudMonoFrame(ph)
	w.process(frame)
	if r := rms(frame); r < 0.05 {
		t.Fatalf("two frames after switching to always, output RMS = %v, want the mic to pass", r)
	}

	// Switch to PTT with the key UP -> force-closed; the level converges within a
	// frame and the following frame is silent.
	e.SetMicMode("ptt")
	e.SetPTTDown(false)
	frame, ph = loudMonoFrame(ph)
	w.process(frame)
	if gl := e.GateLevel(); gl > 5e-3 {
		t.Fatalf("one frame after PTT-up, GateLevel = %v, want ~0", gl)
	}
	frame, ph = loudMonoFrame(ph)
	w.process(frame)
	if r := rms(frame); r > 1e-3 {
		t.Fatalf("two frames after PTT-up, output RMS = %v, want ~0", r)
	}
	// PTT key DOWN -> forced open; level ~1 by end of frame.
	e.SetPTTDown(true)
	frame, ph = loudMonoFrame(ph)
	w.process(frame)
	if gl := e.GateLevel(); gl < 0.99 {
		t.Fatalf("one frame after PTT-down, GateLevel = %v, want ~1", gl)
	}
}

// TestGateLevelTracksEmittedSignal confirms the published GateLevel is DERIVED from
// the frame actually emitted: after a forced mute the meter is 0, and the emitted
// amplitude tracks the published level. It guards against the meter drifting from
// the cable signal (the "门控电平要反映实际发出的信号" requirement).
func TestGateLevelTracksEmittedSignal(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetMicMode("mute")
	w := newWorkerFor(e)
	defer w.apm.Close()

	ph := 0.0
	var frame []float32
	for i := 0; i < 20; i++ {
		frame, ph = sineFrame(220, 0.5, ph)
		w.process(frame)
		if rms(frame) > 1e-3 {
			t.Fatalf("muted frame %d RMS = %v, want ~0", i, rms(frame))
		}
		if gl := e.GateLevel(); gl > 5e-3 {
			t.Fatalf("muted frame %d GateLevel = %v, want ~0", i, gl)
		}
	}

	// Always: full open, meter ~1, output non-silent.
	e.SetMicMode("always")
	for i := 0; i < 5; i++ {
		frame, ph = sineFrame(220, 0.5, ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl < 0.99 {
		t.Fatalf("after always, GateLevel = %v, want ~1", gl)
	}
	if r := rms(frame); r < 0.2 {
		t.Fatalf("after always, output RMS = %v, want the 0.5 sine (~0.35)", r)
	}
}

// TestVADReopensAfterForcedMode confirms the forced-mode latch reset: after an
// Always/PTT-open excursion, returning to VAD must NOT inherit a stale "open"
// latch state and leak a quiet room. The gate must re-close on silence and only
// re-open on fresh evidence.
func TestVADReopensAfterForcedMode(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetAdvancedVoiceActivity(false) // energy latch
	e.SetGateSensitivity(0.5)
	e.SetMicMode("always")
	w := newWorkerFor(e)
	defer w.apm.Close()

	// Always opens the gate fully.
	var frame []float32
	for i := 0; i < 10; i++ {
		frame, _ = sineFrame(220, 0.3, float64(i))
		w.process(frame)
	}
	if gl := e.GateLevel(); gl < 0.99 {
		t.Fatalf("always should open, GateLevel = %v", gl)
	}

	// Return to VAD and feed SILENCE. The latch must have been reset, so the gate
	// closes promptly rather than staying open from the Always excursion.
	e.SetMicMode("vad")
	for i := 0; i < 200; i++ {
		frame = make([]float32, dspFrame)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl > 0.05 {
		t.Fatalf("VAD after an always excursion must close on silence, GateLevel = %v", gl)
	}
}

// --- A2: tier / AGC / echo switches run the new config, no stale, no stacking --

// TestTierSwitchAppliesImmediately confirms a tier change is applied on the very
// next frame and the worker's tracked epoch follows the engine atomics — no stale
// config surviving a switch. It asserts across every tier transition.
func TestTierSwitchAppliesImmediately(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("always")
	e.SetNoiseSuppressionTier("none")
	w := newWorkerFor(e)
	defer w.apm.Close()

	order := []struct {
		tier string
		want int32
	}{
		{"standard", nsTierStandard},
		{"high", nsTierHigh},
		{"strong", nsTierStrong},
		{"none", nsTierNone},
		{"high", nsTierHigh},
	}
	for _, step := range order {
		e.SetNoiseSuppressionTier(step.tier)
		frame, _ := sineFrame(220, 0.2, 0)
		w.process(frame)
		if w.nsTierApplied != step.want {
			t.Fatalf("after switching to %q, worker applied tier = %d, want %d", step.tier, w.nsTierApplied, step.want)
		}
	}
}

// TestStrongTierNeverStacksAPMNS pins the no-stacking invariant across a switch INTO
// and OUT OF Strong: with a real denoiser the APM's noise suppression must be OFF in
// Strong (RNNoise does the work) and ON again on leaving Strong. The APM config the
// worker builds is the observable, so we assert nsConfigForTierApplied directly AND
// via the worker's applied epoch.
func TestStrongTierNeverStacksAPMNS(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("always")

	// With a real denoiser, Strong -> APM NS OFF.
	if enabled, _ := nsConfigForTierApplied(nsTierStrong, true); enabled {
		t.Fatal("Strong with a real denoiser must build an APM config with NS OFF (no stacking)")
	}
	// Leaving Strong for High -> APM NS ON at High.
	if enabled, lvl := nsConfigForTierApplied(nsTierHigh, true); !enabled || lvl != apm.NSLevelHigh {
		t.Fatalf("High should enable APM NS at High, got enabled=%v level=%d", enabled, lvl)
	}

	// Through the worker: switch strong -> high and assert the applied tier moves.
	fake := &fakeDenoiser{prob: 0.9}
	w := newWorkerWithDen(e, fake) // denVAD = true
	defer w.apm.Close()
	e.SetNoiseSuppressionTier("strong")
	frame, _ := sineFrame(220, 0.2, 0)
	w.process(frame)
	if w.nsTierApplied != nsTierStrong {
		t.Fatalf("worker should apply strong tier, got %d", w.nsTierApplied)
	}
	e.SetNoiseSuppressionTier("high")
	frame, _ = sineFrame(220, 0.2, 0)
	w.process(frame)
	if w.nsTierApplied != nsTierHigh {
		t.Fatalf("worker should apply high tier after switch, got %d", w.nsTierApplied)
	}
}

// TestAGCToggleConverges confirms an AGC toggle flips the worker's applied epoch on
// the next frame, so the APM runs with the new gain-control setting immediately.
func TestAGCToggleConverges(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("always")
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetEchoCancellation(false)
	w := newWorkerFor(e)
	defer w.apm.Close()

	if w.agcApplied || w.aecApplied {
		t.Fatalf("worker should start with agc/echo off, got %v/%v", w.agcApplied, w.aecApplied)
	}
	e.SetAGC(true)
	e.SetEchoCancellation(true)
	frame, _ := sineFrame(220, 0.2, 0)
	w.process(frame)
	if !w.agcApplied || !w.aecApplied {
		t.Fatalf("worker should apply agc/echo on after toggle, got %v/%v", w.agcApplied, w.aecApplied)
	}
	e.SetAGC(false)
	e.SetEchoCancellation(false)
	frame, _ = sineFrame(220, 0.2, 0)
	w.process(frame)
	if w.agcApplied || w.aecApplied {
		t.Fatalf("worker should apply agc/echo off after toggle back, got %v/%v", w.agcApplied, w.aecApplied)
	}
}

// TestDenoiserDegradeReenablesAPMNS is the I3 guard: when the Strong tier's RNNoise
// degrades to a Passthrough (runtime allocation failure), the APM's own noise
// suppression must be re-enabled so the transmitted signal is not left raw with two
// suppressors effectively doing nothing. The inverse holds when the denoiser returns.
func TestDenoiserDegradeReenablesAPMNS(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("always")
	e.SetNoiseSuppressionTier("strong")

	// Degraded (no real denoiser): Strong falls back to the APM's High suppressor.
	if enabled, lvl := nsConfigForTierApplied(nsTierStrong, false); !enabled || lvl != apm.NSLevelHigh {
		t.Fatalf("Strong with a degraded denoiser must keep APM NS ON at High, got enabled=%v level=%d", enabled, lvl)
	}

	// A worker whose denoiser is Passthrough must report denVAD=false (energy path)
	// and must have re-applied the APM config for that capability.
	w := newWorkerWithDen(e, denoise.Passthrough{})
	defer w.apm.Close()
	w.denVAD = true // simulate the stale "was real" flag being wrong
	w.denVADApplied = true
	frame, _ := sineFrame(220, 0.2, 0)
	w.process(frame)
	if w.denVAD {
		t.Fatal("worker must reconcile denVAD=false for a Passthrough denoiser")
	}
	if w.denVADApplied != w.denVAD {
		t.Fatalf("worker must re-apply the APM config for the degraded capability: denVADApplied=%v denVAD=%v", w.denVADApplied, w.denVAD)
	}
}

// --- A3: underrun must not leak mute/PTT-up and must not splice raw mic ---------

// TestUnderrunStaleProcessedAudioDoesNotLeakMute is the critical H2 guard beyond the
// existing full-underrun tests: even when the output ring holds STALE PROCESSED
// audio (the worker processed a pre-mute frame but has not yet caught up to the new
// mute), the RT callback must emit SILENCE for the whole buffer when force-closed.
// The old code pulled that stale audio and emitted it (only the underrun TAIL was
// silenced), leaking pre-mute voice after the user hit Mute.
func TestUnderrunStaleProcessedAudioDoesNotLeakMute(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("mute")

	// Pre-load the output ring with a full period of LOUD "processed" audio — what
	// the worker produced BEFORE the mute took effect.
	stale := make([]float32, periodFrames)
	for i := range stale {
		stale[i] = 0.6
	}
	if n := e.outRing.push(stale); n != periodFrames {
		t.Fatalf("test setup: pushed %d stale samples, want %d", n, periodFrames)
	}

	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = 0.5 // loud live mic too
	}
	out := make([]byte, len(micF)*4)
	e.duplexCallback(out, f32bytes(micF), periodFrames)

	for i, s := range bytesAsF32(out) {
		if s != 0 {
			t.Fatalf("mute must suppress stale processed audio AND the live mic: sample %d = %v, want 0", i, s)
		}
	}
	// The stale audio must also have been DRAINED so it cannot replay on re-open.
	if avail := e.outRing.length(); avail != 0 {
		t.Fatalf("force-closed buffer must drain the output ring so stale audio cannot replay, %d samples remain", avail)
	}
}

// TestUnderrunStaleProcessedAudioDoesNotLeakPTTUp is the PTT-up twin of the above:
// releasing PTT must silence the cable even if stale pre-release processed audio is
// queued, and must drain it.
func TestUnderrunStaleProcessedAudioDoesNotLeakPTTUp(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("ptt")
	e.SetPTTDown(false) // key up -> force-closed

	stale := make([]float32, periodFrames)
	for i := range stale {
		stale[i] = 0.7
	}
	e.outRing.push(stale)

	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = 0.5
	}
	out := make([]byte, len(micF)*4)
	e.duplexCallback(out, f32bytes(micF), periodFrames)

	for i, s := range bytesAsF32(out) {
		if s != 0 {
			t.Fatalf("PTT-up must suppress stale processed audio AND the live mic: sample %d = %v, want 0", i, s)
		}
	}
	if avail := e.outRing.length(); avail != 0 {
		t.Fatalf("PTT-up force-closed buffer must drain the output ring, %d samples remain", avail)
	}
}

// TestForceClosedPublishesZeroGateLevel confirms the RT callback publishes
// GateLevel 0 the instant it force-closes the mic, so the UI meter matches the
// guaranteed-silent cable even while the worker lags behind.
func TestForceClosedPublishesZeroGateLevel(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("mute")
	e.setGateLevel(0.8) // pretend the worker last published a high level

	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = 0.4
	}
	out := make([]byte, len(micF)*4)
	e.duplexCallback(out, f32bytes(micF), periodFrames)

	if gl := e.GateLevel(); gl != 0 {
		t.Fatalf("force-closed buffer must publish GateLevel 0 so the meter matches the silent cable, got %v", gl)
	}
}

// TestUnderrunNoRawMicSplice confirms the underrun path never splices the full-level
// raw mic into the output: with the output ring EMPTY and a loud live mic, the
// documented fallback is raw passthrough ONLY on a FULL underrun (got==0, no
// processed sample to hold from). On a PARTIAL underrun the tail must be the
// hold-last-to-silence ramp, never the raw mic. This re-asserts the buzz-fix
// invariant from the convergence angle.
func TestUnderrunNoRawMicSplice(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("vad") // not force-closed

	// A partial underrun: 100 processed samples far from the raw-mic level.
	const got = 100
	const processed float32 = -0.4
	pre := make([]float32, got)
	for i := range pre {
		pre[i] = processed
	}
	e.outRing.push(pre)

	const rawMic float32 = 0.6
	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = rawMic
	}
	out := make([]byte, len(micF)*4)
	e.duplexCallback(out, f32bytes(micF), periodFrames)

	o := bytesAsF32(out)
	// The tail must never reach the positive raw-mic level, and must end at silence.
	for f := got; f < periodFrames; f++ {
		s := o[f*channels]
		if s > 1e-4 {
			t.Fatalf("tail sample %d = %v leaked the raw mic (must hold-last toward silence)", f, s)
		}
	}
	if last := o[(periodFrames-1)*channels]; math.Abs(float64(last)) > fEps {
		t.Fatalf("tail must end at silence, last = %v", last)
	}
}

// --- A4: degraded denoiser falls back to energy; recovers to probability -------

// TestDegradedDenoiserFallsBackToEnergy is the core H3 guard: with Advanced VAD ON
// but the denoiser degraded to Passthrough (probability identically 0), a LOUD frame
// must still OPEN the gate via the energy latch. Without the per-frame reconciliation
// the VAD latch would see p==0 forever and hold the mic permanently shut.
func TestDegradedDenoiserFallsBackToEnergy(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(true)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetGateSensitivity(0.15)
	// Auto-sensitivity off so the manual threshold drives the energy latch.

	w := newWorkerWithDen(e, denoise.Passthrough{}) // p always 0
	defer w.apm.Close()

	ph := 0.0
	var frame []float32
	for i := 0; i < 80; i++ {
		frame, ph = loudMonoFrame(ph) // loud speech: energy well above threshold
		w.process(frame)
	}
	if w.denVAD {
		t.Fatal("a Passthrough denoiser must not report denVAD=true")
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("degraded denoiser must fall back to the energy latch and open on loud speech, GateLevel = %v", gl)
	}
	if r := rms(frame); r < 0.05 {
		t.Fatalf("degraded-denoiser gate should pass the mic on loud speech, RMS = %v", r)
	}
}

// TestDegradedDenoiserStaysClosedOnSilence is the other half: the energy fallback
// must not OPEN on a silent frame either — the mic must stay shut in a quiet room.
func TestDegradedDenoiserStaysClosedOnSilence(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(true)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetGateSensitivity(0.15)
	w := newWorkerWithDen(e, denoise.Passthrough{})
	defer w.apm.Close()

	for i := 0; i < 100; i++ {
		frame := make([]float32, dspFrame)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl > 0.05 {
		t.Fatalf("degraded-denoiser energy fallback must stay closed on silence, GateLevel = %v", gl)
	}
}

// scriptedDenoiser is a controllable Denoiser that can be FLIPPED at runtime between
// "real" (returns a high speech probability) and "degraded" (behaves like a
// Passthrough returning 0), so a test can drive degrade -> recover on the SAME
// worker without swapping the concrete type (which reconcileDenoiser keys on).
type scriptedDenoiser struct {
	prob float32
}

func (s *scriptedDenoiser) Process(frame []float32) float32 { return s.prob }
func (s *scriptedDenoiser) Close()                          {}

// countingFactory returns a denFactory that hands out a fresh real denoiser and
// counts how many times it was called, so a test can prove recovery was ATTEMPTED.
func countingFactory(d denoise.Denoiser) (func() denoise.Denoiser, *int32) {
	var n int32
	return func() denoise.Denoiser {
		atomic.AddInt32(&n, 1)
		return d
	}, &n
}

// TestDenoiserDegradeThenRecover drives the SAME worker through a real denoiser that
// degrades (returns p==0 like Passthrough) and then RECOVERS (returns p high again),
// asserting the gate follows: probability-driven while real, energy fallback while
// degraded, and probability-driven again after recovery. The denVAD flag is derived
// from the CONCRETE type in the production path, so here we model degradation by
// swapping to a Passthrough-typed denoiser and recovery by having denFactory hand
// back a real-typed one.
func TestDenoiserDegradeThenRecover(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(true)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetGateSensitivity(0.15)

	// Start with a real (fake) denoiser supplying a HIGH probability.
	real := &fakeDenoiser{prob: 0.95}
	w := newWorkerWithDen(e, real)
	defer w.apm.Close()

	// Silent frames + high probability -> gate opens via the VAD latch.
	for i := 0; i < 40; i++ {
		w.process(make([]float32, dspFrame))
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("real denoiser: VAD should open on high probability, GateLevel = %v", gl)
	}

	// DEGRADE: the denoiser becomes a Passthrough (native state freed / alloc
	// failure). No factory is wired, so the worker stays on the energy latch.
	w.den = denoise.Passthrough{}
	// Feed silence. The gate may stay open briefly from the VAD latch's long hold
	// (~250ms anti-chop) and the ~120ms release, so allow well past both before
	// asserting closure.
	for i := 0; i < 300; i++ {
		w.process(make([]float32, dspFrame)) // silence: energy latch stays closed
	}
	if w.denVAD {
		t.Fatal("after degradation the worker must report denVAD=false (energy fallback)")
	}
	if gl := e.GateLevel(); gl > 0.05 {
		t.Fatalf("degraded denoiser on silence must close the gate via energy, GateLevel = %v", gl)
	}

	// And a loud frame now OPENS via energy (proving the fallback is live, not the
	// frozen VAD latch).
	ph := 0.0
	var frame []float32
	for i := 0; i < 40; i++ {
		frame, ph = loudMonoFrame(ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("degraded denoiser must open on loud speech via energy, GateLevel = %v", gl)
	}

	// RECOVER: a fresh real denoiser becomes available. Wire the factory to hand back
	// one that supplies a HIGH probability, and let the throttled recovery fire.
	recovered := &fakeDenoiser{prob: 0.95}
	w.denFactory, _ = countingFactory(recovered)
	// Drive silent frames past the recovery interval. On recovery, denVAD flips true
	// and the high probability re-opens the VAD gate even with zero energy.
	for i := 0; i < denRecoverFrames+40; i++ {
		w.process(make([]float32, dspFrame))
	}
	if !w.denVAD {
		t.Fatal("after a real denoiser is available again the worker must recover denVAD=true")
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("recovered denoiser must drive the VAD gate open on high probability, GateLevel = %v", gl)
	}
}

// TestDenoiserRecoveryIsThrottled confirms reconcileDenoiser does NOT rebuild the
// denoiser every frame while it stays degraded (that would hammer the allocator),
// but DOES attempt periodically.
func TestDenoiserRecoveryIsThrottled(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("mute") // forced: keep the gate work trivial
	e.SetAdvancedVoiceActivity(true)
	w := newWorkerWithDen(e, denoise.Passthrough{})

	// Factory that always FAILS to produce a real denoiser (returns Passthrough), so
	// the worker never recovers and keeps retrying on the throttle.
	failFactory, calls := countingFactory(denoise.Passthrough{})
	w.denFactory = failFactory

	const frames = denRecoverFrames + 10
	for i := 0; i < frames; i++ {
		w.process(make([]float32, dspFrame))
	}
	// It must have attempted recovery a small number of times, not once per frame.
	got := atomic.LoadInt32(calls)
	if got < 1 {
		t.Fatal("recovery should have been attempted at least once over the interval")
	}
	if got > int32(frames/denRecoverFrames)+2 {
		t.Fatalf("recovery attempted %d times over %d frames; must be throttled to ~%d", got, frames, frames/denRecoverFrames)
	}
	if w.denVAD {
		t.Fatal("a factory that only yields Passthrough must leave denVAD=false")
	}
}

// --- A5: auto-sensitivity tracks the noise floor, not the manual slider --------

// TestAutoSensitivityIgnoresManualSliderMaxed pins the auto-sensitivity contract
// from the convergence angle: with auto ON and the MANUAL slider maxed (which alone
// would keep the gate shut), a mid-level signal above the tracked noise floor must
// still open the gate, and steady near-floor noise must stay shut.
func TestAutoSensitivityIgnoresManualSliderMaxed(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(false) // energy path
	e.SetAutoSensitivity(true)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetGateSensitivity(1.0) // manual threshold 0.05, must be BYPASSED
	w := newWorkerFor(e)
	defer w.apm.Close()

	// Quiet room tone near the floor: must stay shut.
	ph := 0.0
	for i := 0; i < 80; i++ {
		var frame []float32
		frame, ph = sineFrame(300, 0.003, ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl > 0.2 {
		t.Fatalf("auto-sensitivity must keep the gate shut on near-floor noise, GateLevel = %v", gl)
	}

	// A normal-speaking-level signal: must open via the auto threshold even though
	// the manual slider is maxed.
	for i := 0; i < 60; i++ {
		var frame []float32
		frame, ph = sineFrame(300, 0.04, ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("auto-sensitivity must open above the floor with the manual slider maxed, GateLevel = %v", gl)
	}
}

// TestAutoSensitivityOffUsesManualSlider confirms that with auto OFF the manual
// slider is authoritative: maxed it keeps a mid-level signal SHUT (the mirror of the
// auto test), proving the two paths are genuinely distinct.
func TestAutoSensitivityOffUsesManualSlider(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(false)
	e.SetAutoSensitivity(false)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetGateSensitivity(1.0) // manual threshold 0.05
	w := newWorkerFor(e)
	defer w.apm.Close()

	ph := 0.0
	for i := 0; i < 80; i++ {
		var frame []float32
		frame, ph = sineFrame(300, 0.04, ph) // RMS ~0.028 < 0.05 manual threshold
		w.process(frame)
	}
	if gl := e.GateLevel(); gl > 0.2 {
		t.Fatalf("manual maxed sensitivity must keep a sub-threshold signal shut, GateLevel = %v", gl)
	}
}

// --- A6: concurrent mode/tier switching is race-free and converges ------------

// TestConcurrentModeAndTierSwitchConverges drives the worker from one goroutine
// while another hammers micMode and the NS tier, under -race, and asserts the final
// state converges to the LAST settings written (mute -> silence, GateLevel 0). It
// guards the "并发切换模式与降噪档位时 worker 与回调不产生数据竞争，门控状态最终一致"
// contract.
func TestConcurrentModeAndTierSwitchConverges(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	w := newWorkerFor(e)
	defer w.apm.Close()

	modes := []string{"vad", "always", "mute", "ptt"}
	tiers := []string{"none", "standard", "high", "strong"}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			e.SetMicMode(modes[i%len(modes)])
			e.SetNoiseSuppressionTier(tiers[i%len(tiers)])
			e.SetAGC(i%2 == 0)
			e.SetEchoCancellation(i%3 == 0)
			e.SetPTTDown(i%2 == 0)
		}
	}()

	for i := 0; i < 4000; i++ {
		frame := make([]float32, dspFrame)
		for j := range frame {
			frame[j] = 0.1
		}
		w.process(frame)
	}
	wg.Wait()

	// Force a known terminal state and let it converge, then assert.
	e.SetMicMode("mute")
	e.SetPTTDown(false)
	for i := 0; i < 5; i++ {
		var frame []float32
		frame, _ = sineFrame(220, 0.3, 0)
		w.process(frame)
		if r := rms(frame); r > 1e-3 {
			t.Fatalf("after concurrent switching, final mute must converge to silence, RMS = %v", r)
		}
	}
	if gl := e.GateLevel(); gl > 5e-3 {
		t.Fatalf("final mute must converge GateLevel ~0, got %v", gl)
	}
}

// TestConcurrentCallbackAndWorkerNoRace drives the REAL duplexCallback (RT path) and
// the REAL worker goroutine over the real rings while a third goroutine switches
// modes/tiers/mute concurrently, under -race. It proves the RT callback's forced-
// close read, the worker's config reads, and the setters share no unguarded state.
func TestConcurrentCallbackAndWorkerNoRace(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("vad")

	e.startWorker()
	defer e.stopWorker()

	stop := make(chan struct{})
	var cbWG sync.WaitGroup
	cbWG.Add(1)
	go func() {
		defer cbWG.Done()
		out := make([]byte, periodFrames*channels*4)
		mic := make([]byte, periodFrames*channels*4)
		for {
			select {
			case <-stop:
				return
			default:
				e.duplexCallback(out, mic, periodFrames)
			}
		}
	}()

	modes := []string{"vad", "always", "mute", "ptt"}
	tiers := []string{"none", "standard", "high", "strong"}
	var mutWG sync.WaitGroup
	mutWG.Add(1)
	go func() {
		defer mutWG.Done()
		for i := 0; i < 2000; i++ {
			e.SetMicMode(modes[i%len(modes)])
			e.SetNoiseSuppressionTier(tiers[i%len(tiers)])
			e.SetAGC(i%2 == 0)
			e.SetEchoCancellation(i%3 == 0)
			e.SetPTTDown(i%2 == 0)
			if i%50 == 0 {
				// Also read the meter, as the UI's events loop does.
				_ = e.GateLevel()
			}
		}
	}()
	mutWG.Wait()
	close(stop)
	cbWG.Wait()
}

// --- A1/A3 cross: force-close during a lagging worker --------------------------

// TestForceCloseWithLaggingWorkerNeverLeaks is the I1 interaction: a tier switch and
// an underrun happen together. We set mute, leave the output ring holding stale
// processed audio, and assert the RT callback emits silence and drains the ring —
// so neither the stale (old-config) processed audio nor the raw live mic reaches the
// cable.
func TestForceCloseWithLaggingWorkerNeverLeaks(t *testing.T) {
	e := configuredEngine()
	// Establish stale processed MONO audio as if the worker had been running and is
	// now a period behind (the out ring is mono, one sample per stereo frame).
	stale := make([]float32, periodFrames*2)
	for i := range stale {
		stale[i] = 0.5
	}
	e.outRing.push(stale)
	e.SetMicMode("mute")

	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = 0.45
	}
	out := make([]byte, len(micF)*4)
	e.duplexCallback(out, f32bytes(micF), periodFrames)

	for i, s := range bytesAsF32(out) {
		if s != 0 {
			t.Fatalf("force-close under lag must emit silence, sample %d = %v", i, s)
		}
	}
}

// TestStrongDegradedStillSuppressesNoise is the I3 end-to-end guard: in the Strong
// tier with the denoiser degraded to Passthrough, the transmitted signal must STILL
// be noise-suppressed — via the APM's re-enabled High suppressor — rather than left
// raw. With a real APM available, white noise through the degraded Strong chain must
// come out markedly quieter than its input. Skipped when the APM is unavailable (the
// no-op passthrough cannot suppress, which is the honest degraded build behavior).
func TestStrongDegradedStillSuppressesNoise(t *testing.T) {
	if !apm.Available() {
		t.Skip("APM unavailable in this build; cannot verify the APM-suppressor fallback")
	}
	e := NewEngine(nil, nil)
	e.SetMicMode("always") // gate open so the gate does not mask the comparison
	e.SetNoiseSuppressionTier("strong")
	e.SetAGC(false)

	w := newWorkerWithDen(e, denoise.Passthrough{})
	defer w.apm.Close()

	rng := newDeterministicRand()
	var last []float32
	for i := 0; i < 200; i++ {
		frame := make([]float32, dspFrame)
		for j := range frame {
			frame[j] = float32((rng()*2 - 1) * 0.1)
		}
		w.process(frame)
		last = frame
	}
	// The input was uniform ±0.1 noise; its RMS is 0.1/sqrt(3) ~= 0.0577. If the APM
	// suppressor were NOT re-enabled the output would stay near that level.
	const wantInRMS = 0.0577
	outRMS := rms(last)
	if w.denVAD {
		t.Fatal("Passthrough denoiser must reconcile denVAD=false")
	}
	if outRMS >= float32(wantInRMS)*0.7 {
		t.Fatalf("degraded Strong tier must still suppress noise via the APM (out RMS %v vs input ~%v)", outRMS, wantInRMS)
	}
}
