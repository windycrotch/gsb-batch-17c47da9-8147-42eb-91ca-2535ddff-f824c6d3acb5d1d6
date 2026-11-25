package wgpu

import (
	"fmt"
	"unsafe"

	"github.com/gogpu/gputypes"
)

// CommandEncoderDescriptor describes a command encoder to create.
type CommandEncoderDescriptor struct {
	Label string
}

// commandEncoderDescriptorWire is the FFI-compatible C-layout struct for wgpu-native.
// nextInChain(8)+label(16) = 24 bytes.
type commandEncoderDescriptorWire struct {
	NextInChain uintptr    // *ChainedStruct
	Label       StringView // 16 bytes
}

// CommandBufferDescriptor describes a command buffer.
type CommandBufferDescriptor struct {
	NextInChain uintptr // *ChainedStruct
	Label       StringView
}

// ComputePassTimestampWrites is a deprecated alias for PassTimestampWrites.
// Deprecated: Use PassTimestampWrites. Renamed in wgpu-native v29.
type ComputePassTimestampWrites = PassTimestampWrites

// computePassDescriptorWire is the native FFI structure for ComputePassDescriptor.
// v29: timestampWrites field is *WGPUPassTimestampWrites (unified, not separate ComputePassTimestampWrites).
type computePassDescriptorWire struct {
	nextInChain     uintptr    // 8 bytes
	label           StringView // 16 bytes
	timestampWrites uintptr    // 8 bytes (*passTimestampWrites, nullable)
}

// ComputePassDescriptor describes a compute pass (user-facing API).
type ComputePassDescriptor struct {
	Label           string
	TimestampWrites *PassTimestampWrites // optional; use PassTimestampWrites (was ComputePassTimestampWrites)
}

// CreateCommandEncoder creates a command encoder.
// Returns an error if the FFI call fails or the device is nil.
func (d *Device) CreateCommandEncoder(desc *CommandEncoderDescriptor) (*CommandEncoder, error) {
	if err := checkInit(); err != nil {
		return nil, err
	}
	if d == nil || d.handle == 0 {
		return nil, &WGPUError{Op: "CreateCommandEncoder", Message: "device is nil or released"}
	}
	var descPtr uintptr
	if desc != nil {
		wire := commandEncoderDescriptorWire{
			Label: stringToStringView(desc.Label),
		}
		descPtr = uintptr(unsafe.Pointer(&wire))
	}
	handle, _, _ := procDeviceCreateCommandEncoder.Call(
		d.handle,
		descPtr,
	)
	if handle == 0 {
		return nil, &WGPUError{Op: "CreateCommandEncoder", Message: "wgpu returned null handle"}
	}
	trackResource(handle, "CommandEncoder")
	return &CommandEncoder{handle: handle}, nil
}

// BeginComputePass begins a compute pass.
// Returns an error if the FFI call fails or the encoder is nil.
func (enc *CommandEncoder) BeginComputePass(desc *ComputePassDescriptor) (*ComputePassEncoder, error) {
	if err := checkInit(); err != nil {
		return nil, err
	}
	if enc == nil || enc.handle == 0 {
		return nil, &WGPUError{Op: "BeginComputePass", Message: "encoder is nil or released"}
	}
	if err := enc.beginPass("BeginComputePass"); err != nil {
		return nil, err
	}

	var wireDesc computePassDescriptorWire
	var wireTimestamp passTimestampWrites
	var descPtr uintptr

	if desc != nil {
		wireDesc.nextInChain = 0
		if desc.Label != "" {
			labelBytes := []byte(desc.Label)
			wireDesc.label = StringView{
				Data:   uintptr(unsafe.Pointer(&labelBytes[0])),
				Length: uintptr(len(labelBytes)),
			}
		} else {
			wireDesc.label = EmptyStringView()
		}
		if desc.TimestampWrites != nil {
			wireTimestamp = passTimestampWrites{
				nextInChain:               0,
				querySet:                  desc.TimestampWrites.QuerySet.handle,
				beginningOfPassWriteIndex: desc.TimestampWrites.BeginningOfPassWriteIndex,
				endOfPassWriteIndex:       desc.TimestampWrites.EndOfPassWriteIndex,
			}
			wireDesc.timestampWrites = uintptr(unsafe.Pointer(&wireTimestamp))
		}
		descPtr = uintptr(unsafe.Pointer(&wireDesc))
	}

	handle, _, _ := procCommandEncoderBeginComputePass.Call(
		enc.handle,
		descPtr,
	)
	if handle == 0 {
		enc.abortPass()
		return nil, &WGPUError{Op: "BeginComputePass", Message: "wgpu returned null handle"}
	}
	trackResource(handle, "ComputePassEncoder")
	return &ComputePassEncoder{handle: handle, encoder: enc}, nil
}

// CopyBufferToBuffer copies data between buffers.
func (enc *CommandEncoder) CopyBufferToBuffer(src *Buffer, srcOffset uint64, dst *Buffer, dstOffset uint64, size uint64) error {
	mustInit()
	const op = "CommandEncoder.CopyBufferToBuffer"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	if src == nil || src.handle == 0 || dst == nil || dst.handle == 0 {
		return &WGPUError{Op: op, Message: "source or destination buffer is nil or released"}
	}
	procCommandEncoderCopyBufferToBuffer.Call( //nolint:errcheck
		enc.handle,
		src.handle,
		uintptr(srcOffset),
		dst.handle,
		uintptr(dstOffset),
		uintptr(size),
	)
	return nil
}

// ClearBuffer clears a region of a buffer to zeros.
// size = 0 means clear from offset to end of buffer.
func (enc *CommandEncoder) ClearBuffer(buffer *Buffer, offset, size uint64) error {
	mustInit()
	const op = "CommandEncoder.ClearBuffer"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	if buffer == nil || buffer.handle == 0 {
		return &WGPUError{Op: op, Message: "buffer is nil or released"}
	}
	procCommandEncoderClearBuffer.Call( //nolint:errcheck
		enc.handle,
		buffer.handle,
		uintptr(offset),
		uintptr(size),
	)
	return nil
}

// InsertDebugMarker inserts a single debug marker label.
// This is useful for GPU debugging tools to identify specific command points.
func (enc *CommandEncoder) InsertDebugMarker(markerLabel string) error {
	mustInit()
	const op = "CommandEncoder.InsertDebugMarker"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	labelBytes := []byte(markerLabel)
	if len(labelBytes) == 0 {
		return nil
	}
	label := StringView{
		Data:   uintptr(unsafe.Pointer(&labelBytes[0])),
		Length: uintptr(len(labelBytes)),
	}
	procCommandEncoderInsertDebugMarker.Call( //nolint:errcheck
		enc.handle,
		uintptr(unsafe.Pointer(&label)),
	)
	return nil
}

// PushDebugGroup begins a labeled debug group.
// Use PopDebugGroup to end the group. Groups can be nested.
func (enc *CommandEncoder) PushDebugGroup(groupLabel string) error {
	mustInit()
	const op = "CommandEncoder.PushDebugGroup"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	labelBytes := []byte(groupLabel)
	if len(labelBytes) == 0 {
		return nil
	}
	label := StringView{
		Data:   uintptr(unsafe.Pointer(&labelBytes[0])),
		Length: uintptr(len(labelBytes)),
	}
	procCommandEncoderPushDebugGroup.Call( //nolint:errcheck
		enc.handle,
		uintptr(unsafe.Pointer(&label)),
	)
	return nil
}

// PopDebugGroup ends the current debug group.
// Must match a preceding PushDebugGroup call.
func (enc *CommandEncoder) PopDebugGroup() error {
	mustInit()
	const op = "CommandEncoder.PopDebugGroup"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	procCommandEncoderPopDebugGroup.Call(enc.handle) //nolint:errcheck
	return nil
}

// CopyBufferToTexture copies data from a buffer to a texture using low-level wire types.
// Errors are reported via Device error scopes, not as return values.
func (enc *CommandEncoder) CopyBufferToTexture(source *TexelCopyBufferInfo, destination *TexelCopyTextureInfo, copySize *gputypes.Extent3D) error {
	mustInit()
	const op = "CommandEncoder.CopyBufferToTexture"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	if source == nil || destination == nil || copySize == nil {
		return &WGPUError{Op: op, Message: "source, destination and copySize are required"}
	}
	procCommandEncoderCopyBufferToTexture.Call( //nolint:errcheck
		enc.handle,
		uintptr(unsafe.Pointer(source)),
		uintptr(unsafe.Pointer(destination)),
		uintptr(unsafe.Pointer(copySize)),
	)
	return nil
}

// CopyTextureToBuffer copies data from a texture to a buffer.
// Accepts gogpu/wgpu-compatible types: src *Texture, dst *Buffer, regions []BufferTextureCopy.
// Each region specifies the buffer layout, texture subresource origin, and copy extent.
// Errors are reported via Device error scopes, not as return values.
func (enc *CommandEncoder) CopyTextureToBuffer(src *Texture, dst *Buffer, regions []BufferTextureCopy) error {
	mustInit()
	const op = "CommandEncoder.CopyTextureToBuffer"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	if src == nil || dst == nil || len(regions) == 0 {
		return &WGPUError{Op: op, Message: "source, destination and at least one region are required"}
	}
	for i := range regions {
		r := &regions[i]
		srcWire := r.TextureBase.toWire()
		dstWire := TexelCopyBufferInfo{
			Layout: TexelCopyBufferLayout{
				Offset:       r.BufferLayout.Offset,
				BytesPerRow:  r.BufferLayout.BytesPerRow,
				RowsPerImage: r.BufferLayout.RowsPerImage,
			},
			Buffer: dst.handle,
		}
		size := r.Size
		procCommandEncoderCopyTextureToBuffer.Call( //nolint:errcheck
			enc.handle,
			uintptr(unsafe.Pointer(&srcWire)),
			uintptr(unsafe.Pointer(&dstWire)),
			uintptr(unsafe.Pointer(&size)),
		)
	}
	return nil
}

// CopyTextureToBufferRaw copies data from a texture to a buffer using low-level wire types.
// Prefer [CopyTextureToBuffer] for new code.
func (enc *CommandEncoder) CopyTextureToBufferRaw(source *TexelCopyTextureInfo, destination *TexelCopyBufferInfo, copySize *gputypes.Extent3D) error {
	mustInit()
	const op = "CommandEncoder.CopyTextureToBufferRaw"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	if source == nil || destination == nil || copySize == nil {
		return &WGPUError{Op: op, Message: "source, destination and copySize are required"}
	}
	procCommandEncoderCopyTextureToBuffer.Call( //nolint:errcheck
		enc.handle,
		uintptr(unsafe.Pointer(source)),
		uintptr(unsafe.Pointer(destination)),
		uintptr(unsafe.Pointer(copySize)),
	)
	return nil
}

// CopyTextureToTexture copies data from one texture to another.
// Accepts gogpu/wgpu-compatible types: src *Texture, dst *Texture, regions []TextureCopy.
// Each region specifies the source and destination subresource origins and copy extent.
// Errors are reported via Device error scopes, not as return values.
func (enc *CommandEncoder) CopyTextureToTexture(src, dst *Texture, regions []TextureCopy) error {
	mustInit()
	const op = "CommandEncoder.CopyTextureToTexture"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	if src == nil || dst == nil || len(regions) == 0 {
		return &WGPUError{Op: op, Message: "source, destination and at least one region are required"}
	}
	for i := range regions {
		r := &regions[i]
		srcWire := r.Source.toWire()
		dstWire := r.Destination.toWire()
		size := r.Size
		procCommandEncoderCopyTextureToTexture.Call( //nolint:errcheck
			enc.handle,
			uintptr(unsafe.Pointer(&srcWire)),
			uintptr(unsafe.Pointer(&dstWire)),
			uintptr(unsafe.Pointer(&size)),
		)
	}
	return nil
}

// CopyTextureToTextureRaw copies data from one texture to another using low-level wire types.
// Prefer [CopyTextureToTexture] for new code.
func (enc *CommandEncoder) CopyTextureToTextureRaw(source *TexelCopyTextureInfo, destination *TexelCopyTextureInfo, copySize *gputypes.Extent3D) error {
	mustInit()
	const op = "CommandEncoder.CopyTextureToTextureRaw"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	if source == nil || destination == nil || copySize == nil {
		return &WGPUError{Op: op, Message: "source, destination and copySize are required"}
	}
	procCommandEncoderCopyTextureToTexture.Call( //nolint:errcheck
		enc.handle,
		uintptr(unsafe.Pointer(source)),
		uintptr(unsafe.Pointer(destination)),
		uintptr(unsafe.Pointer(copySize)),
	)
	return nil
}

// Finish finishes recording and returns a command buffer.
// The optional desc argument allows setting a label; pass nothing for defaults.
// This variadic signature matches the gogpu/wgpu API for compatibility.
//
// Finish fails if a pass begun on this encoder is still open (ErrPassOpen) —
// in that case the encoder becomes invalid and can never be submitted. It
// also fails if the encoder was already finished, released or invalidated;
// repeated calls are idempotent and never reach the native layer twice.
func (enc *CommandEncoder) Finish(desc ...*CommandBufferDescriptor) (*CommandBuffer, error) {
	if err := checkInit(); err != nil {
		return nil, err
	}
	const op = "CommandEncoder.Finish"
	if enc == nil {
		return nil, &WGPUError{Op: op, Message: "encoder is nil"}
	}
	enc.mu.Lock()
	switch enc.state {
	case encoderFinished:
		enc.mu.Unlock()
		return nil, lifecycleError(op, ErrRecordingFinished)
	case encoderReleased:
		enc.mu.Unlock()
		return nil, lifecycleError(op, ErrReleased)
	case encoderInvalid:
		enc.mu.Unlock()
		return nil, lifecycleError(op, ErrEncoderInvalid)
	}
	if enc.openPass {
		// Illegal call sequence: poison the encoder so it can never be submitted.
		enc.state = encoderInvalid
		enc.mu.Unlock()
		return nil, lifecycleError(op, ErrPassOpen)
	}
	handle := enc.handle
	if handle != 0 {
		// Mark finished up front so concurrent Finish calls cannot both
		// reach the native layer; rolled back if the native call fails.
		enc.state = encoderFinished
	}
	enc.mu.Unlock()
	if handle == 0 {
		return nil, &WGPUError{Op: op, Message: "encoder is released"}
	}

	var descPtr uintptr
	if len(desc) > 0 && desc[0] != nil {
		descPtr = uintptr(unsafe.Pointer(desc[0]))
	}
	cbHandle, _, _ := procCommandEncoderFinish.Call(
		handle,
		descPtr,
	)
	if cbHandle == 0 {
		enc.mu.Lock()
		enc.state = encoderRecording
		enc.mu.Unlock()
		return nil, &WGPUError{Op: op, Message: "wgpu returned null handle"}
	}
	trackResource(cbHandle, "CommandBuffer")
	return &CommandBuffer{handle: cbHandle}, nil
}

// Release releases the command encoder.
// It is idempotent: the native release runs at most once.
func (enc *CommandEncoder) Release() {
	if enc == nil {
		return
	}
	enc.mu.Lock()
	if enc.state == encoderReleased {
		enc.mu.Unlock()
		return
	}
	enc.state = encoderReleased
	handle := enc.handle
	enc.handle = 0
	enc.mu.Unlock()

	if handle != 0 {
		untrackResource(handle)
		procCommandEncoderRelease.Call(handle) //nolint:errcheck
	}
}

// WriteTimestamp writes a timestamp to a query.
// Note: This is a wgpu-native extension. Prefer pass-level timestamps
// via RenderPassTimestampWrites or ComputePassTimestampWrites when possible.
func (enc *CommandEncoder) WriteTimestamp(querySet *QuerySet, queryIndex uint32) error {
	mustInit()
	const op = "CommandEncoder.WriteTimestamp"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	if querySet == nil || querySet.handle == 0 {
		return &WGPUError{Op: op, Message: "query set is nil or released"}
	}
	procCommandEncoderWriteTimestamp.Call( //nolint:errcheck
		enc.handle,
		querySet.handle,
		uintptr(queryIndex),
	)
	return nil
}

// ResolveQuerySet resolves query results to a buffer.
// The buffer must have BufferUsageQueryResolve usage.
func (enc *CommandEncoder) ResolveQuerySet(querySet *QuerySet, firstQuery, queryCount uint32, destination *Buffer, destinationOffset uint64) error {
	mustInit()
	const op = "CommandEncoder.ResolveQuerySet"
	if err := enc.checkRecording(op); err != nil {
		return err
	}
	if querySet == nil || querySet.handle == 0 || destination == nil || destination.handle == 0 {
		return &WGPUError{Op: op, Message: "query set or destination buffer is nil or released"}
	}
	procCommandEncoderResolveQuerySet.Call( //nolint:errcheck
		enc.handle,
		querySet.handle,
		uintptr(firstQuery),
		uintptr(queryCount),
		destination.handle,
		uintptr(destinationOffset),
	)
	return nil
}

// Handle returns the underlying handle.
func (enc *CommandEncoder) Handle() uintptr { return enc.handle }

// SetPipeline sets the compute pipeline.
func (cpe *ComputePassEncoder) SetPipeline(pipeline *ComputePipeline) error {
	mustInit()
	const op = "ComputePassEncoder.SetPipeline"
	if err := cpe.lockRecording(op); err != nil {
		return err
	}
	defer cpe.mu.Unlock()
	if pipeline == nil || pipeline.handle == 0 {
		return lifecycleError(op, ErrDependencyReleased)
	}
	procComputePassEncoderSetPipeline.Call( //nolint:errcheck
		cpe.handle,
		pipeline.handle,
	)
	return nil
}

// SetBindGroup sets a bind group.
func (cpe *ComputePassEncoder) SetBindGroup(groupIndex uint32, group *BindGroup, dynamicOffsets []uint32) error {
	mustInit()
	const op = "ComputePassEncoder.SetBindGroup"
	if err := cpe.lockRecording(op); err != nil {
		return err
	}
	defer cpe.mu.Unlock()
	if group == nil || group.handle == 0 {
		return lifecycleError(op, ErrDependencyReleased)
	}
	var offsetsPtr uintptr
	offsetCount := uintptr(0)
	if len(dynamicOffsets) > 0 {
		offsetsPtr = uintptr(unsafe.Pointer(&dynamicOffsets[0]))
		offsetCount = uintptr(len(dynamicOffsets))
	}
	procComputePassEncoderSetBindGroup.Call( //nolint:errcheck
		cpe.handle,
		uintptr(groupIndex),
		group.handle,
		offsetCount,
		offsetsPtr,
	)
	return nil
}

// DispatchWorkgroups dispatches compute work.
func (cpe *ComputePassEncoder) DispatchWorkgroups(x, y, z uint32) error {
	mustInit()
	const op = "ComputePassEncoder.DispatchWorkgroups"
	if err := cpe.lockRecording(op); err != nil {
		return err
	}
	defer cpe.mu.Unlock()
	procComputePassEncoderDispatchWorkgroups.Call( //nolint:errcheck
		cpe.handle,
		uintptr(x),
		uintptr(y),
		uintptr(z),
	)
	return nil
}

// DispatchWorkgroupsIndirect dispatches compute work using parameters from a GPU buffer.
// indirectBuffer must contain a DispatchIndirectArgs structure:
//   - workgroupCountX (uint32)
//   - workgroupCountY (uint32)
//   - workgroupCountZ (uint32)
func (cpe *ComputePassEncoder) DispatchWorkgroupsIndirect(indirectBuffer *Buffer, indirectOffset uint64) error {
	mustInit()
	const op = "ComputePassEncoder.DispatchWorkgroupsIndirect"
	if err := cpe.lockRecording(op); err != nil {
		return err
	}
	defer cpe.mu.Unlock()
	if indirectBuffer == nil || indirectBuffer.handle == 0 {
		return lifecycleError(op, ErrDependencyReleased)
	}
	procComputePassEncoderDispatchWorkgroupsIndirect.Call( //nolint:errcheck
		cpe.handle,
		indirectBuffer.handle,
		uintptr(indirectOffset),
	)
	return nil
}

// End ends the compute pass.
// The first call ends the pass in the native layer; subsequent calls return
// ErrPassEnded without touching the native layer again.
func (cpe *ComputePassEncoder) End() error {
	mustInit()
	const op = "ComputePassEncoder.End"
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
	cpe.ended = true
	handle := cpe.handle
	enc := cpe.encoder
	cpe.mu.Unlock()

	if handle != 0 {
		procComputePassEncoderEnd.Call(handle) //nolint:errcheck
	}
	if enc != nil {
		enc.passEnded()
	}
	return nil
}

// Release releases the compute pass encoder.
// It is idempotent: the native release runs at most once. Releasing a pass
// that was never ended invalidates its parent command encoder.
func (cpe *ComputePassEncoder) Release() {
	if cpe == nil {
		return
	}
	cpe.mu.Lock()
	if cpe.released {
		cpe.mu.Unlock()
		return
	}
	cpe.released = true
	wasEnded := cpe.ended
	handle := cpe.handle
	cpe.handle = 0
	enc := cpe.encoder
	cpe.encoder = nil
	cpe.mu.Unlock()

	if handle != 0 {
		untrackResource(handle)
		procComputePassEncoderRelease.Call(handle) //nolint:errcheck
	}
	if enc != nil && !wasEnded {
		enc.passAbandoned()
	}
}

// Handle returns the underlying handle.
func (cpe *ComputePassEncoder) Handle() uintptr { return cpe.handle }

// Submit submits command buffers for execution.
// Returns the submission index (uint64) and nil on success. The submission
// index can be used with Device.Poll to track when work completes.
// Matches gogpu/wgpu Queue.Submit(commands ...*CommandBuffer) (uint64, error).
//
// If any command buffer is nil or released, the whole submission is rejected
// with an error and nothing is sent to the native queue (no partial submit).
func (q *Queue) Submit(commands ...*CommandBuffer) (uint64, error) {
	mustInit()
	if q == nil || q.handle == 0 || len(commands) == 0 {
		return 0, nil
	}
	handles := make([]uintptr, len(commands))
	for i, cmd := range commands {
		if cmd == nil || cmd.handle == 0 {
			return 0, lifecycleError(fmt.Sprintf("Queue.Submit (command buffer %d)", i), ErrReleased)
		}
		handles[i] = cmd.handle
	}
	// wgpuQueueSubmitForIndex is a wgpu-native extension that returns WGPUSubmissionIndex (uint64).
	// This enables callers to poll for GPU completion of a specific submission.
	submissionIndex, _, _ := procQueueSubmitForIndex.Call(
		q.handle,
		uintptr(len(handles)),
		uintptr(unsafe.Pointer(&handles[0])),
	)
	return uint64(submissionIndex), nil
}

// GetTimestampPeriod returns the duration of one GPU timestamp tick in
// nanoseconds, as reported by wgpu-native. It returns zero for a nil or
// released queue, or when the native call is unavailable.
func (q *Queue) GetTimestampPeriod() float32 {
	if q == nil || q.handle == 0 {
		return 0
	}
	if procQueueGetTimestampPeriod == nil {
		return 0
	}

	proc, ok := procQueueGetTimestampPeriod.(float32Proc)
	if !ok {
		return 0
	}
	period, err := proc.CallFloat32(q.handle)
	if err != nil {
		return 0
	}
	return period
}

// Release releases the command buffer.
// It is idempotent: the native release runs at most once.
func (cb *CommandBuffer) Release() {
	if cb == nil || cb.handle == 0 {
		return
	}
	untrackResource(cb.handle)
	procCommandBufferRelease.Call(cb.handle) //nolint:errcheck
	cb.handle = 0
}

// Handle returns the underlying handle.
func (cb *CommandBuffer) Handle() uintptr { return cb.handle }
