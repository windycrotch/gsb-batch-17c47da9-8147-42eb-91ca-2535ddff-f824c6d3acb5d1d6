// lifecycle_test.go verifies the lifecycle rules of RenderPassEncoder,
// CommandEncoder, RenderBundleEncoder and RenderBundle WITHOUT a real GPU:
// the native procedure table is replaced by recording mocks, so rejection
// and idempotency behavior can be asserted offline.
package wgpu

import (
	"errors"
	"sync"
	"testing"

	"github.com/gogpu/gputypes"
)

// mockProc is a Proc that records its calls and returns a fixed value.
type mockProc struct {
	mu    sync.Mutex
	calls [][]uintptr
	ret   uintptr
}

func (m *mockProc) Call(args ...uintptr) (uintptr, uintptr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, append([]uintptr(nil), args...))
	return m.ret, 0, nil
}

func (m *mockProc) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// lifecycleMock holds the mocked native procedures used by lifecycle tests.
type lifecycleMock struct {
	createCommandEncoder  *mockProc
	beginRenderPass       *mockProc
	beginComputePass      *mockProc
	passSetPipeline       *mockProc
	passSetVertexBuffer   *mockProc
	passDraw              *mockProc
	passEnd               *mockProc
	passRelease           *mockProc
	passExecuteBundles    *mockProc
	computePassEnd        *mockProc
	computePassRelease    *mockProc
	encoderFinish         *mockProc
	encoderRelease        *mockProc
	commandBufferRelease  *mockProc
	queueSubmit           *mockProc
	createBundleEncoder   *mockProc
	bundleEncSetPipeline  *mockProc
	bundleEncDraw         *mockProc
	bundleEncFinish       *mockProc
	bundleEncRelease      *mockProc
	bundleRelease         *mockProc
	renderPipelineRelease *mockProc
	bufferRelease         *mockProc
}

// installLifecycleMock replaces the native procedure table with mocks and
// bypasses native library loading. All state is restored via t.Cleanup.
func installLifecycleMock(t *testing.T) *lifecycleMock {
	t.Helper()

	m := &lifecycleMock{
		createCommandEncoder:  &mockProc{ret: 1000},
		beginRenderPass:       &mockProc{ret: 2000},
		beginComputePass:      &mockProc{ret: 2100},
		passSetPipeline:       &mockProc{},
		passSetVertexBuffer:   &mockProc{},
		passDraw:              &mockProc{},
		passEnd:               &mockProc{},
		passRelease:           &mockProc{},
		passExecuteBundles:    &mockProc{},
		computePassEnd:        &mockProc{},
		computePassRelease:    &mockProc{},
		encoderFinish:         &mockProc{ret: 3000},
		encoderRelease:        &mockProc{},
		commandBufferRelease:  &mockProc{},
		queueSubmit:           &mockProc{ret: 42},
		createBundleEncoder:   &mockProc{ret: 4000},
		bundleEncSetPipeline:  &mockProc{},
		bundleEncDraw:         &mockProc{},
		bundleEncFinish:       &mockProc{ret: 5000},
		bundleEncRelease:      &mockProc{},
		bundleRelease:         &mockProc{},
		renderPipelineRelease: &mockProc{},
		bufferRelease:         &mockProc{},
	}

	// Bypass Init(): consume the sync.Once so Init() reports success without
	// loading the native library. Restore a fresh Once afterwards so other
	// tests can initialize for real.
	initOnce = sync.Once{}
	initOnce.Do(func() {})
	initErr = nil
	t.Cleanup(func() {
		initOnce = sync.Once{}
		initErr = nil
	})

	swap := func(slot *Proc, p Proc) {
		orig := *slot
		*slot = p
		t.Cleanup(func() { *slot = orig })
	}
	swap(&procDeviceCreateCommandEncoder, m.createCommandEncoder)
	swap(&procCommandEncoderBeginRenderPass, m.beginRenderPass)
	swap(&procCommandEncoderBeginComputePass, m.beginComputePass)
	swap(&procRenderPassEncoderSetPipeline, m.passSetPipeline)
	swap(&procRenderPassEncoderSetVertexBuffer, m.passSetVertexBuffer)
	swap(&procRenderPassEncoderDraw, m.passDraw)
	swap(&procRenderPassEncoderEnd, m.passEnd)
	swap(&procRenderPassEncoderRelease, m.passRelease)
	swap(&procRenderPassEncoderExecuteBundles, m.passExecuteBundles)
	swap(&procComputePassEncoderEnd, m.computePassEnd)
	swap(&procComputePassEncoderRelease, m.computePassRelease)
	swap(&procCommandEncoderFinish, m.encoderFinish)
	swap(&procCommandEncoderRelease, m.encoderRelease)
	swap(&procCommandBufferRelease, m.commandBufferRelease)
	swap(&procQueueSubmitForIndex, m.queueSubmit)
	swap(&procDeviceCreateRenderBundleEncoder, m.createBundleEncoder)
	swap(&procRenderBundleEncoderSetPipeline, m.bundleEncSetPipeline)
	swap(&procRenderBundleEncoderDraw, m.bundleEncDraw)
	swap(&procRenderBundleEncoderFinish, m.bundleEncFinish)
	swap(&procRenderBundleEncoderRelease, m.bundleEncRelease)
	swap(&procRenderBundleRelease, m.bundleRelease)
	swap(&procRenderPipelineRelease, m.renderPipelineRelease)
	swap(&procBufferRelease, m.bufferRelease)

	return m
}

// newMockEncoder creates a command encoder backed by the mock device.
func newMockEncoder(t *testing.T, m *lifecycleMock) *CommandEncoder {
	t.Helper()
	d := &Device{handle: 1}
	enc, err := d.CreateCommandEncoder(nil)
	if err != nil {
		t.Fatalf("CreateCommandEncoder: %v", err)
	}
	if enc == nil || enc.Handle() != 1000 {
		t.Fatalf("unexpected encoder: %v", enc)
	}
	return enc
}

// beginMockPass begins a render pass on the encoder.
func beginMockPass(t *testing.T, enc *CommandEncoder) *RenderPassEncoder {
	t.Helper()
	pass, err := enc.BeginRenderPass(&RenderPassDescriptor{
		ColorAttachments: []RenderPassColorAttachment{
			{View: &TextureView{handle: 50}},
		},
	})
	if err != nil {
		t.Fatalf("BeginRenderPass: %v", err)
	}
	if pass == nil {
		t.Fatal("BeginRenderPass returned nil pass")
	}
	return pass
}

// newMockBundle records a simple render bundle via the mock device.
func newMockBundle(t *testing.T, m *lifecycleMock) *RenderBundle {
	t.Helper()
	d := &Device{handle: 1}
	rbe, err := d.CreateRenderBundleEncoder(&RenderBundleEncoderDescriptor{
		ColorFormats: []gputypes.TextureFormat{gputypes.TextureFormatBGRA8Unorm},
	})
	if err != nil {
		t.Fatalf("CreateRenderBundleEncoder: %v", err)
	}
	if err := rbe.Draw(3, 1, 0, 0); err != nil {
		t.Fatalf("RenderBundleEncoder.Draw: %v", err)
	}
	bundle, err := rbe.Finish()
	if err != nil {
		t.Fatalf("RenderBundleEncoder.Finish: %v", err)
	}
	if bundle == nil {
		t.Fatal("Finish returned nil bundle")
	}
	return bundle
}

// TestLifecycleNormalFrame verifies the normal path is unaffected: record a
// bundle, execute it in a pass, end the pass, finish and submit.
func TestLifecycleNormalFrame(t *testing.T) {
	m := installLifecycleMock(t)

	bundle := newMockBundle(t, m)

	enc := newMockEncoder(t, m)
	pass := beginMockPass(t, enc)

	pipeline := &RenderPipeline{handle: 9001}
	vb := &Buffer{handle: 9002}
	if err := pass.SetPipeline(pipeline); err != nil {
		t.Fatalf("SetPipeline: %v", err)
	}
	if err := pass.SetVertexBuffer(0, vb, 0, 64); err != nil {
		t.Fatalf("SetVertexBuffer: %v", err)
	}
	if err := pass.Draw(3, 1, 0, 0); err != nil {
		t.Fatalf("Draw: %v", err)
	}
	// A bundle can be executed multiple times.
	if err := pass.ExecuteBundles([]*RenderBundle{bundle}); err != nil {
		t.Fatalf("ExecuteBundles #1: %v", err)
	}
	if err := pass.ExecuteBundles([]*RenderBundle{bundle}); err != nil {
		t.Fatalf("ExecuteBundles #2: %v", err)
	}
	if err := pass.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	pass.Release()

	cb, err := enc.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	enc.Release()

	q := &Queue{handle: 7}
	idx, err := q.Submit(cb)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if idx != 42 {
		t.Fatalf("submission index = %d, want 42", idx)
	}
	cb.Release()

	// Every native call happened exactly once (twice for ExecuteBundles).
	if got := m.passSetPipeline.callCount(); got != 1 {
		t.Errorf("native SetPipeline calls = %d, want 1", got)
	}
	if got := m.passDraw.callCount(); got != 1 {
		t.Errorf("native Draw calls = %d, want 1", got)
	}
	if got := m.passExecuteBundles.callCount(); got != 2 {
		t.Errorf("native ExecuteBundles calls = %d, want 2", got)
	}
	if got := m.passEnd.callCount(); got != 1 {
		t.Errorf("native End calls = %d, want 1", got)
	}
	if got := m.encoderFinish.callCount(); got != 1 {
		t.Errorf("native Finish calls = %d, want 1", got)
	}
	if got := m.queueSubmit.callCount(); got != 1 {
		t.Errorf("native Submit calls = %d, want 1", got)
	}
}

// TestLifecyclePassEndThenRecordRejected verifies that after End, all
// recording calls on the pass are rejected and never reach the native layer.
func TestLifecyclePassEndThenRecordRejected(t *testing.T) {
	m := installLifecycleMock(t)
	enc := newMockEncoder(t, m)
	pass := beginMockPass(t, enc)

	if err := pass.End(); err != nil {
		t.Fatalf("End: %v", err)
	}

	if err := pass.SetPipeline(&RenderPipeline{handle: 1}); !errors.Is(err, ErrPassEnded) {
		t.Errorf("SetPipeline after End: got %v, want ErrPassEnded", err)
	}
	if err := pass.SetVertexBuffer(0, &Buffer{handle: 1}, 0, 0); !errors.Is(err, ErrPassEnded) {
		t.Errorf("SetVertexBuffer after End: got %v, want ErrPassEnded", err)
	}
	if err := pass.Draw(3, 1, 0, 0); !errors.Is(err, ErrPassEnded) {
		t.Errorf("Draw after End: got %v, want ErrPassEnded", err)
	}
	if err := pass.ExecuteBundles([]*RenderBundle{{handle: 5}}); !errors.Is(err, ErrPassEnded) {
		t.Errorf("ExecuteBundles after End: got %v, want ErrPassEnded", err)
	}

	// Nothing reached the native layer.
	if got := m.passSetPipeline.callCount(); got != 0 {
		t.Errorf("native SetPipeline calls = %d, want 0", got)
	}
	if got := m.passDraw.callCount(); got != 0 {
		t.Errorf("native Draw calls = %d, want 0", got)
	}
	if got := m.passExecuteBundles.callCount(); got != 0 {
		t.Errorf("native ExecuteBundles calls = %d, want 0", got)
	}
}

// TestLifecycleDoubleEndIdempotent verifies repeated End calls do not invoke
// the native End twice.
func TestLifecycleDoubleEndIdempotent(t *testing.T) {
	m := installLifecycleMock(t)
	enc := newMockEncoder(t, m)
	pass := beginMockPass(t, enc)

	if err := pass.End(); err != nil {
		t.Fatalf("first End: %v", err)
	}
	if err := pass.End(); !errors.Is(err, ErrPassEnded) {
		t.Fatalf("second End: got %v, want ErrPassEnded", err)
	}
	if got := m.passEnd.callCount(); got != 1 {
		t.Errorf("native End calls = %d, want 1", got)
	}
}

// TestLifecycleDoubleReleaseIdempotent verifies repeated Release calls on
// every recording object invoke the native release at most once.
func TestLifecycleDoubleReleaseIdempotent(t *testing.T) {
	m := installLifecycleMock(t)

	enc := newMockEncoder(t, m)
	pass := beginMockPass(t, enc)
	if err := pass.End(); err != nil {
		t.Fatalf("End: %v", err)
	}

	pass.Release()
	pass.Release()
	if got := m.passRelease.callCount(); got != 1 {
		t.Errorf("native pass Release calls = %d, want 1", got)
	}

	enc.Release()
	enc.Release()
	if got := m.encoderRelease.callCount(); got != 1 {
		t.Errorf("native encoder Release calls = %d, want 1", got)
	}

	bundle := newMockBundle(t, m)
	bundle.Release()
	bundle.Release()
	if got := m.bundleRelease.callCount(); got != 1 {
		t.Errorf("native bundle Release calls = %d, want 1", got)
	}

	d := &Device{handle: 1}
	rbe, err := d.CreateRenderBundleEncoder(&RenderBundleEncoderDescriptor{
		ColorFormats: []gputypes.TextureFormat{gputypes.TextureFormatBGRA8Unorm},
	})
	if err != nil {
		t.Fatalf("CreateRenderBundleEncoder: %v", err)
	}
	rbe.Release()
	rbe.Release()
	if got := m.bundleEncRelease.callCount(); got != 1 {
		t.Errorf("native bundle encoder Release calls = %d, want 1", got)
	}
}

// TestLifecycleFinishWithOpenPass verifies that finishing an encoder while a
// pass is still open fails and poisons the encoder for submission.
func TestLifecycleFinishWithOpenPass(t *testing.T) {
	m := installLifecycleMock(t)
	enc := newMockEncoder(t, m)
	_ = beginMockPass(t, enc) // pass never ended

	if _, err := enc.Finish(); !errors.Is(err, ErrPassOpen) {
		t.Fatalf("Finish with open pass: got %v, want ErrPassOpen", err)
	}
	if got := m.encoderFinish.callCount(); got != 0 {
		t.Errorf("native Finish calls = %d, want 0", got)
	}

	// The encoder is now invalid: it can never be finished/submitted.
	if _, err := enc.Finish(); !errors.Is(err, ErrEncoderInvalid) {
		t.Fatalf("second Finish: got %v, want ErrEncoderInvalid", err)
	}
	if _, err := enc.BeginRenderPass(&RenderPassDescriptor{
		ColorAttachments: []RenderPassColorAttachment{{View: &TextureView{handle: 51}}},
	}); !errors.Is(err, ErrEncoderInvalid) {
		t.Fatalf("BeginRenderPass after invalidation: got %v, want ErrEncoderInvalid", err)
	}
	if got := m.encoderFinish.callCount(); got != 0 {
		t.Errorf("native Finish calls = %d, want 0", got)
	}
}

// TestLifecycleEncoderOpsAfterFinish verifies recording calls after Finish
// are rejected and Finish itself is idempotent at the native layer.
func TestLifecycleEncoderOpsAfterFinish(t *testing.T) {
	m := installLifecycleMock(t)
	enc := newMockEncoder(t, m)

	cb, err := enc.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if cb == nil {
		t.Fatal("Finish returned nil command buffer")
	}

	if _, err := enc.Finish(); !errors.Is(err, ErrRecordingFinished) {
		t.Errorf("second Finish: got %v, want ErrRecordingFinished", err)
	}
	if err := enc.CopyBufferToBuffer(&Buffer{handle: 1}, 0, &Buffer{handle: 2}, 0, 4); !errors.Is(err, ErrRecordingFinished) {
		t.Errorf("CopyBufferToBuffer after Finish: got %v, want ErrRecordingFinished", err)
	}
	if err := enc.ClearBuffer(&Buffer{handle: 1}, 0, 0); !errors.Is(err, ErrRecordingFinished) {
		t.Errorf("ClearBuffer after Finish: got %v, want ErrRecordingFinished", err)
	}
	if _, err := enc.BeginRenderPass(&RenderPassDescriptor{
		ColorAttachments: []RenderPassColorAttachment{{View: &TextureView{handle: 52}}},
	}); !errors.Is(err, ErrRecordingFinished) {
		t.Errorf("BeginRenderPass after Finish: got %v, want ErrRecordingFinished", err)
	}
	if got := m.encoderFinish.callCount(); got != 1 {
		t.Errorf("native Finish calls = %d, want 1", got)
	}
}

// TestLifecycleEncoderReleaseThenOps verifies operations on a released
// encoder are rejected.
func TestLifecycleEncoderReleaseThenOps(t *testing.T) {
	m := installLifecycleMock(t)
	enc := newMockEncoder(t, m)
	enc.Release()

	if _, err := enc.Finish(); !errors.Is(err, ErrReleased) {
		t.Errorf("Finish after Release: got %v, want ErrReleased", err)
	}
	if err := enc.ClearBuffer(&Buffer{handle: 1}, 0, 0); !errors.Is(err, ErrReleased) {
		t.Errorf("ClearBuffer after Release: got %v, want ErrReleased", err)
	}
	if got := m.encoderFinish.callCount(); got != 0 {
		t.Errorf("native Finish calls = %d, want 0", got)
	}
}

// TestLifecyclePassReleaseThenOpsRejected verifies that after Release, pass
// operations fail with ErrReleased.
func TestLifecyclePassReleaseThenOpsRejected(t *testing.T) {
	m := installLifecycleMock(t)
	enc := newMockEncoder(t, m)
	pass := beginMockPass(t, enc)
	pass.Release()

	if err := pass.Draw(3, 1, 0, 0); !errors.Is(err, ErrReleased) {
		t.Errorf("Draw after Release: got %v, want ErrReleased", err)
	}
	if err := pass.End(); !errors.Is(err, ErrReleased) {
		t.Errorf("End after Release: got %v, want ErrReleased", err)
	}
	if got := m.passDraw.callCount(); got != 0 {
		t.Errorf("native Draw calls = %d, want 0", got)
	}
	if got := m.passEnd.callCount(); got != 0 {
		t.Errorf("native End calls = %d, want 0", got)
	}
}

// TestLifecyclePassReleasedWithoutEndPoisonsEncoder verifies that releasing
// a live (un-ended) pass invalidates its parent encoder.
func TestLifecyclePassReleasedWithoutEndPoisonsEncoder(t *testing.T) {
	m := installLifecycleMock(t)
	enc := newMockEncoder(t, m)
	pass := beginMockPass(t, enc)
	pass.Release() // released without End

	if _, err := enc.Finish(); !errors.Is(err, ErrEncoderInvalid) {
		t.Fatalf("Finish after pass abandoned: got %v, want ErrEncoderInvalid", err)
	}
	if got := m.encoderFinish.callCount(); got != 0 {
		t.Errorf("native Finish calls = %d, want 0", got)
	}
}

// TestLifecycleDependencyReleaseFailsDraw verifies that releasing a resource
// a live pass depends on makes subsequent draws fail instead of crashing.
func TestLifecycleDependencyReleaseFailsDraw(t *testing.T) {
	m := installLifecycleMock(t)

	t.Run("pipeline", func(t *testing.T) {
		enc := newMockEncoder(t, m)
		pass := beginMockPass(t, enc)
		pipeline := &RenderPipeline{handle: 9101}
		if err := pass.SetPipeline(pipeline); err != nil {
			t.Fatalf("SetPipeline: %v", err)
		}
		pipeline.Release() // release while pass is alive

		if err := pass.Draw(3, 1, 0, 0); !errors.Is(err, ErrDependencyReleased) {
			t.Fatalf("Draw after pipeline release: got %v, want ErrDependencyReleased", err)
		}
		if got := m.passDraw.callCount(); got != 0 {
			t.Errorf("native Draw calls = %d, want 0", got)
		}
	})

	t.Run("vertex buffer", func(t *testing.T) {
		enc := newMockEncoder(t, m)
		pass := beginMockPass(t, enc)
		vb := &Buffer{handle: 9102}
		if err := pass.SetVertexBuffer(0, vb, 0, 64); err != nil {
			t.Fatalf("SetVertexBuffer: %v", err)
		}
		vb.Release() // release while pass is alive

		if err := pass.Draw(3, 1, 0, 0); !errors.Is(err, ErrDependencyReleased) {
			t.Fatalf("Draw after vertex buffer release: got %v, want ErrDependencyReleased", err)
		}
		if got := m.passDraw.callCount(); got != 0 {
			t.Errorf("native Draw calls = %d, want 0", got)
		}
	})

	t.Run("set with released resource", func(t *testing.T) {
		enc := newMockEncoder(t, m)
		pass := beginMockPass(t, enc)
		pipeline := &RenderPipeline{handle: 9103}
		pipeline.Release()

		before := m.passSetPipeline.callCount()
		if err := pass.SetPipeline(pipeline); !errors.Is(err, ErrDependencyReleased) {
			t.Fatalf("SetPipeline with released pipeline: got %v, want ErrDependencyReleased", err)
		}
		if got := m.passSetPipeline.callCount(); got != before {
			t.Errorf("native SetPipeline calls = %d, want %d (unchanged)", got, before)
		}
	})
}

// TestLifecycleReleasePassThenResources verifies the safe release order:
// release the pass first, then its dependencies.
func TestLifecycleReleasePassThenResources(t *testing.T) {
	m := installLifecycleMock(t)
	enc := newMockEncoder(t, m)
	pass := beginMockPass(t, enc)

	pipeline := &RenderPipeline{handle: 9201}
	vb := &Buffer{handle: 9202}
	if err := pass.SetPipeline(pipeline); err != nil {
		t.Fatalf("SetPipeline: %v", err)
	}
	if err := pass.SetVertexBuffer(0, vb, 0, 64); err != nil {
		t.Fatalf("SetVertexBuffer: %v", err)
	}
	if err := pass.End(); err != nil {
		t.Fatalf("End: %v", err)
	}

	// Safe order: pass first, then resources. Must not panic.
	pass.Release()
	pipeline.Release()
	vb.Release()

	if got := m.passRelease.callCount(); got != 1 {
		t.Errorf("native pass Release calls = %d, want 1", got)
	}
	if got := m.renderPipelineRelease.callCount(); got != 1 {
		t.Errorf("native pipeline Release calls = %d, want 1", got)
	}
	if got := m.bufferRelease.callCount(); got != 1 {
		t.Errorf("native buffer Release calls = %d, want 1", got)
	}
}

// TestLifecycleBundleEncoderAfterFinish verifies the render bundle encoder
// rejects recording after Finish and that Finish is idempotent.
func TestLifecycleBundleEncoderAfterFinish(t *testing.T) {
	m := installLifecycleMock(t)
	d := &Device{handle: 1}
	rbe, err := d.CreateRenderBundleEncoder(&RenderBundleEncoderDescriptor{
		ColorFormats: []gputypes.TextureFormat{gputypes.TextureFormatBGRA8Unorm},
	})
	if err != nil {
		t.Fatalf("CreateRenderBundleEncoder: %v", err)
	}
	if err := rbe.SetPipeline(&RenderPipeline{handle: 9301}); err != nil {
		t.Fatalf("SetPipeline: %v", err)
	}
	if err := rbe.Draw(3, 1, 0, 0); err != nil {
		t.Fatalf("Draw: %v", err)
	}

	bundle, err := rbe.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if bundle == nil {
		t.Fatal("Finish returned nil bundle")
	}

	if err := rbe.Draw(3, 1, 0, 0); !errors.Is(err, ErrRecordingFinished) {
		t.Errorf("Draw after Finish: got %v, want ErrRecordingFinished", err)
	}
	if err := rbe.SetPipeline(&RenderPipeline{handle: 9302}); !errors.Is(err, ErrRecordingFinished) {
		t.Errorf("SetPipeline after Finish: got %v, want ErrRecordingFinished", err)
	}
	if _, err := rbe.Finish(); !errors.Is(err, ErrRecordingFinished) {
		t.Errorf("second Finish: got %v, want ErrRecordingFinished", err)
	}

	if got := m.bundleEncFinish.callCount(); got != 1 {
		t.Errorf("native bundle Finish calls = %d, want 1", got)
	}
	if got := m.bundleEncDraw.callCount(); got != 1 {
		t.Errorf("native bundle Draw calls = %d, want 1", got)
	}
	if got := m.bundleEncSetPipeline.callCount(); got != 1 {
		t.Errorf("native bundle SetPipeline calls = %d, want 1", got)
	}
}

// TestLifecycleExecuteBundlesAtomicRejection verifies that ExecuteBundles
// rejects the whole call when any bundle is released, with no partial
// execution reaching the native layer.
func TestLifecycleExecuteBundlesAtomicRejection(t *testing.T) {
	m := installLifecycleMock(t)

	good := newMockBundle(t, m)
	released := newMockBundle(t, m)
	released.Release()

	enc := newMockEncoder(t, m)
	pass := beginMockPass(t, enc)

	// Mixed slice: one live, one released -> whole call rejected.
	if err := pass.ExecuteBundles([]*RenderBundle{good, released}); !errors.Is(err, ErrReleased) {
		t.Fatalf("ExecuteBundles with released bundle: got %v, want ErrReleased", err)
	}
	if got := m.passExecuteBundles.callCount(); got != 0 {
		t.Fatalf("native ExecuteBundles calls = %d, want 0 (no partial execution)", got)
	}

	// Nil bundle is rejected the same way.
	if err := pass.ExecuteBundles([]*RenderBundle{good, nil}); !errors.Is(err, ErrReleased) {
		t.Fatalf("ExecuteBundles with nil bundle: got %v, want ErrReleased", err)
	}
	if got := m.passExecuteBundles.callCount(); got != 0 {
		t.Fatalf("native ExecuteBundles calls = %d, want 0", got)
	}

	// All-live bundles execute fine, repeatedly.
	if err := pass.ExecuteBundles([]*RenderBundle{good, good}); err != nil {
		t.Fatalf("ExecuteBundles with live bundles: %v", err)
	}
	if got := m.passExecuteBundles.callCount(); got != 1 {
		t.Fatalf("native ExecuteBundles calls = %d, want 1", got)
	}
}

// TestLifecycleSubmitReleasedCommandBuffer verifies Submit rejects released
// command buffers atomically.
func TestLifecycleSubmitReleasedCommandBuffer(t *testing.T) {
	m := installLifecycleMock(t)
	enc := newMockEncoder(t, m)
	cb, err := enc.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	cb.Release()

	q := &Queue{handle: 7}
	if _, err := q.Submit(cb); !errors.Is(err, ErrReleased) {
		t.Fatalf("Submit released command buffer: got %v, want ErrReleased", err)
	}
	if got := m.queueSubmit.callCount(); got != 0 {
		t.Errorf("native Submit calls = %d, want 0", got)
	}
}

// TestLifecycleComputePassTracking verifies the encoder tracks compute
// passes the same way: Finish while a compute pass is open fails, and End
// releases the encoder for finishing.
func TestLifecycleComputePassTracking(t *testing.T) {
	m := installLifecycleMock(t)

	// Finish with an open compute pass -> error, encoder poisoned.
	enc := newMockEncoder(t, m)
	if _, err := enc.BeginComputePass(nil); err != nil {
		t.Fatalf("BeginComputePass: %v", err)
	}
	if _, err := enc.Finish(); !errors.Is(err, ErrPassOpen) {
		t.Fatalf("Finish with open compute pass: got %v, want ErrPassOpen", err)
	}
	if _, err := enc.Finish(); !errors.Is(err, ErrEncoderInvalid) {
		t.Fatalf("second Finish: got %v, want ErrEncoderInvalid", err)
	}

	// Normal sequence: begin, end, finish.
	enc2 := newMockEncoder(t, m)
	cpass2, err := enc2.BeginComputePass(nil)
	if err != nil {
		t.Fatalf("BeginComputePass: %v", err)
	}
	if err := cpass2.End(); err != nil {
		t.Fatalf("compute pass End: %v", err)
	}
	if err := cpass2.End(); !errors.Is(err, ErrPassEnded) {
		t.Fatalf("second compute pass End: got %v, want ErrPassEnded", err)
	}
	if got := m.computePassEnd.callCount(); got != 1 {
		t.Errorf("native compute End calls = %d, want 1", got)
	}
	if _, err := enc2.Finish(); err != nil {
		t.Fatalf("Finish after compute pass ended: %v", err)
	}
}

// TestLifecycleSecondPassWhileOpen verifies that beginning a second pass
// while one is still open is rejected.
func TestLifecycleSecondPassWhileOpen(t *testing.T) {
	m := installLifecycleMock(t)
	enc := newMockEncoder(t, m)
	_ = beginMockPass(t, enc)

	if _, err := enc.BeginRenderPass(&RenderPassDescriptor{
		ColorAttachments: []RenderPassColorAttachment{{View: &TextureView{handle: 53}}},
	}); !errors.Is(err, ErrPassOpen) {
		t.Fatalf("second BeginRenderPass: got %v, want ErrPassOpen", err)
	}
	if got := m.beginRenderPass.callCount(); got != 1 {
		t.Errorf("native BeginRenderPass calls = %d, want 1", got)
	}
}
