// lifecycle.go implements explicit lifecycle tracking for recording objects:
// RenderPassEncoder, ComputePassEncoder, CommandEncoder, RenderBundleEncoder
// and RenderBundle.
//
// Once an object is ended, finished or released, any further recording,
// drawing, ending or submitting is rejected with a detectable error
// (errors.Is against the sentinels below) instead of forwarding an invalid
// handle to the native layer. Repeated End/Finish/Release calls are
// idempotent: the native function is invoked at most once.
package wgpu

import (
	"errors"
	"fmt"
)

// Sentinel errors for lifecycle violations. Use errors.Is to match them.
var (
	// ErrRecordingFinished is returned when recording commands on an encoder
	// (command encoder or render bundle encoder) that was already finished.
	ErrRecordingFinished = errors.New("encoder has already been finished")

	// ErrPassEnded is returned when recording on a pass encoder that was
	// already ended, or when ending it a second time.
	ErrPassEnded = errors.New("pass has already been ended")

	// ErrPassOpen is returned when an operation requires no open pass, but a
	// pass begun on this encoder has not been ended yet.
	ErrPassOpen = errors.New("a pass is still open on this encoder")

	// ErrEncoderInvalid is returned when a command encoder was poisoned by an
	// illegal call sequence (e.g. Finish while a pass was still open) and can
	// no longer produce a submittable command buffer.
	ErrEncoderInvalid = errors.New("command encoder is invalid and cannot be submitted")

	// ErrReleased is returned when operating on an object that was released,
	// or when submitting/executing a released command buffer or render bundle.
	ErrReleased = errors.New("object has been released")

	// ErrDependencyReleased is returned when a resource a live pass depends
	// on (pipeline, buffer, bind group) was released while the pass is still
	// recording.
	ErrDependencyReleased = errors.New("a resource used by this pass has been released")
)

// lifecycleError wraps a lifecycle sentinel with the operation name.
func lifecycleError(op string, sentinel error) error {
	return fmt.Errorf("wgpu: %s: %w", op, sentinel)
}

// encoderState is the lifecycle state of a CommandEncoder.
type encoderState int

const (
	encoderRecording encoderState = iota
	encoderFinished
	encoderReleased
	encoderInvalid
)

// beginPass marks the encoder as having an open pass. It fails if the encoder
// is no longer recording or another pass is still open.
func (enc *CommandEncoder) beginPass(op string) error {
	enc.mu.Lock()
	defer enc.mu.Unlock()
	switch enc.state {
	case encoderFinished:
		return lifecycleError(op, ErrRecordingFinished)
	case encoderReleased:
		return lifecycleError(op, ErrReleased)
	case encoderInvalid:
		return lifecycleError(op, ErrEncoderInvalid)
	}
	if enc.openPass {
		return lifecycleError(op, ErrPassOpen)
	}
	enc.openPass = true
	return nil
}

// abortPass rolls back a failed beginPass (native call returned null).
func (enc *CommandEncoder) abortPass() {
	enc.mu.Lock()
	enc.openPass = false
	enc.mu.Unlock()
}

// passEnded is called by a pass encoder when it is ended normally.
func (enc *CommandEncoder) passEnded() {
	enc.mu.Lock()
	enc.openPass = false
	enc.mu.Unlock()
}

// passAbandoned is called when a pass is released without being ended.
// The encoder can no longer produce a valid command buffer.
func (enc *CommandEncoder) passAbandoned() {
	enc.mu.Lock()
	enc.openPass = false
	if enc.state == encoderRecording {
		enc.state = encoderInvalid
	}
	enc.mu.Unlock()
}

// checkRecording verifies the encoder accepts new commands: it must be in
// recording state and have no open pass.
func (enc *CommandEncoder) checkRecording(op string) error {
	if enc == nil {
		return &WGPUError{Op: op, Message: "encoder is nil"}
	}
	enc.mu.Lock()
	defer enc.mu.Unlock()
	switch enc.state {
	case encoderFinished:
		return lifecycleError(op, ErrRecordingFinished)
	case encoderReleased:
		return lifecycleError(op, ErrReleased)
	case encoderInvalid:
		return lifecycleError(op, ErrEncoderInvalid)
	}
	if enc.openPass {
		return lifecycleError(op, ErrPassOpen)
	}
	return nil
}

// lockRecording locks the render pass encoder and verifies it is still
// recording. On success the caller must call rpe.mu.Unlock (directly or via
// defer). On error the lock is not held.
func (rpe *RenderPassEncoder) lockRecording(op string) error {
	if rpe == nil {
		return &WGPUError{Op: op, Message: "render pass encoder is nil"}
	}
	rpe.mu.Lock()
	if rpe.released {
		rpe.mu.Unlock()
		return lifecycleError(op, ErrReleased)
	}
	if rpe.ended {
		rpe.mu.Unlock()
		return lifecycleError(op, ErrPassEnded)
	}
	return nil
}

// checkDependenciesLocked verifies that every resource bound to this pass is
// still alive. rpe.mu must be held. This turns "release a pipeline/buffer
// while the pass is alive" into a clean error on the next draw instead of a
// native crash.
func (rpe *RenderPassEncoder) checkDependenciesLocked(op string) error {
	if rpe.pipeline != nil && rpe.pipeline.handle == 0 {
		return lifecycleError(op+" (render pipeline)", ErrDependencyReleased)
	}
	for slot, b := range rpe.vertexBuffers {
		if b != nil && b.handle == 0 {
			return lifecycleError(fmt.Sprintf("%s (vertex buffer slot %d)", op, slot), ErrDependencyReleased)
		}
	}
	if rpe.indexBuffer != nil && rpe.indexBuffer.handle == 0 {
		return lifecycleError(op+" (index buffer)", ErrDependencyReleased)
	}
	for idx, g := range rpe.bindGroups {
		if g != nil && g.handle == 0 {
			return lifecycleError(fmt.Sprintf("%s (bind group %d)", op, idx), ErrDependencyReleased)
		}
	}
	return nil
}

// lockRecording locks the compute pass encoder and verifies it is still
// recording. On success the caller must call cpe.mu.Unlock. On error the
// lock is not held.
func (cpe *ComputePassEncoder) lockRecording(op string) error {
	if cpe == nil {
		return &WGPUError{Op: op, Message: "compute pass encoder is nil"}
	}
	cpe.mu.Lock()
	if cpe.released {
		cpe.mu.Unlock()
		return lifecycleError(op, ErrReleased)
	}
	if cpe.ended {
		cpe.mu.Unlock()
		return lifecycleError(op, ErrPassEnded)
	}
	return nil
}

// lockRecording locks the render bundle encoder and verifies it is still
// recording. On success the caller must call rbe.mu.Unlock. On error the
// lock is not held.
func (rbe *RenderBundleEncoder) lockRecording(op string) error {
	if rbe == nil {
		return &WGPUError{Op: op, Message: "render bundle encoder is nil"}
	}
	rbe.mu.Lock()
	if rbe.released {
		rbe.mu.Unlock()
		return lifecycleError(op, ErrReleased)
	}
	if rbe.finished {
		rbe.mu.Unlock()
		return lifecycleError(op, ErrRecordingFinished)
	}
	return nil
}

// liveHandle returns the bundle's native handle, or 0 if nil or released.
func (rb *RenderBundle) liveHandle() uintptr {
	if rb == nil {
		return 0
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.handle
}
