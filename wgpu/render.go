package wgpu

import (
	"math"
	"unsafe"

	"github.com/gogpu/gputypes"
)

// DepthSliceUndefined is used for 2D textures in color attachments.
const DepthSliceUndefined uint32 = 0xFFFFFFFF

// TimestampLocationUndefined indicates no timestamp write at this location.
const TimestampLocationUndefined uint32 = 0xFFFFFFFF

// Color represents RGBA color with double precision.
type Color struct {
	R, G, B, A float64
}

// renderPassColorAttachment is the native structure for color attachments.
// Uses uint32 for LoadOp/StoreOp with wgpu-native converted values.
type renderPassColorAttachment struct {
	nextInChain   uintptr // 8 bytes
	view          uintptr // 8 bytes (WGPUTextureView)
	depthSlice    uint32  // 4 bytes - MUST be DepthSliceUndefined for 2D!
	_pad1         [4]byte // 4 bytes padding
	resolveTarget uintptr // 8 bytes (WGPUTextureView, nullable)
	loadOp        uint32  // 4 bytes - wgpu-native converted value
	storeOp       uint32  // 4 bytes - wgpu-native converted value
	clearValue    Color   // 32 bytes (4 * float64)
}

// renderPassDescriptor is the native structure for render pass descriptor.
type renderPassDescriptor struct {
	nextInChain            uintptr    // 8 bytes
	label                  StringView // 16 bytes
	colorAttachmentCount   uintptr    // 8 bytes (size_t)
	colorAttachments       uintptr    // 8 bytes (pointer to array)
	depthStencilAttachment uintptr    // 8 bytes (nullable)
	occlusionQuerySet      uintptr    // 8 bytes (nullable)
	timestampWrites        uintptr    // 8 bytes (nullable)
}

// RenderPassColorAttachment describes a color attachment for a render pass.
type RenderPassColorAttachment struct {
	View          *TextureView
	ResolveTarget *TextureView // For MSAA, nil otherwise
	LoadOp        gputypes.LoadOp
	StoreOp       gputypes.StoreOp
	ClearValue    Color
}

// RenderPassDepthStencilAttachment describes a depth/stencil attachment (user API).
type RenderPassDepthStencilAttachment struct {
	View              *TextureView
	DepthLoadOp       gputypes.LoadOp
	DepthStoreOp      gputypes.StoreOp
	DepthClearValue   float32
	DepthReadOnly     bool
	StencilLoadOp     gputypes.LoadOp
	StencilStoreOp    gputypes.StoreOp
	StencilClearValue uint32
	StencilReadOnly   bool
}

// renderPassDepthStencilAttachment is the native structure (40 bytes).
// Uses uint32 for LoadOp/StoreOp with wgpu-native converted values.
type renderPassDepthStencilAttachment struct {
	view              uintptr
	depthLoadOp       uint32 // wgpu-native converted value
	depthStoreOp      uint32 // wgpu-native converted value
	depthClearValue   float32
	depthReadOnly     Bool
	stencilLoadOp     uint32 // wgpu-native converted value
	stencilStoreOp    uint32 // wgpu-native converted value
	stencilClearValue uint32
	stencilReadOnly   Bool
}

// passTimestampWrites is the native structure for v29 WGPUPassTimestampWrites.
// v29: nextInChain added as FIRST field; unifies former RenderPassTimestampWrites and ComputePassTimestampWrites.
// Layout: nextInChain(8) + querySet(8) + beginningOfPassWriteIndex(4) + endOfPassWriteIndex(4) = 24 bytes.
type passTimestampWrites struct {
	nextInChain               uintptr // 8 bytes (NEW in v29)
	querySet                  uintptr // 8 bytes (WGPUQuerySet)
	beginningOfPassWriteIndex uint32  // 4 bytes
	endOfPassWriteIndex       uint32  // 4 bytes
}

// PassTimestampWrites describes timestamp writes for a render or compute pass.
// v29: Unified struct — replaces separate RenderPassTimestampWrites and ComputePassTimestampWrites.
type PassTimestampWrites struct {
	QuerySet                  *QuerySet
	BeginningOfPassWriteIndex uint32 // Use TimestampLocationUndefined to disable
	EndOfPassWriteIndex       uint32 // Use TimestampLocationUndefined to disable
}

// RenderPassTimestampWrites is a deprecated alias for PassTimestampWrites.
// Deprecated: Use PassTimestampWrites. Renamed in wgpu-native v29.
type RenderPassTimestampWrites = PassTimestampWrites

// RenderPassDescriptor describes a render pass.
type RenderPassDescriptor struct {
	Label                  string
	ColorAttachments       []RenderPassColorAttachment
	DepthStencilAttachment *RenderPassDepthStencilAttachment
	TimestampWrites        *RenderPassTimestampWrites
}

// BeginRenderPass begins a render pass.
// Returns an error if the FFI call fails, encoder is nil, or desc has no color attachments.
func (enc *CommandEncoder) BeginRenderPass(desc *RenderPassDescriptor) (*RenderPassEncoder, error) {
	if err := checkInit(); err != nil {
		return nil, err
	}
	if enc == nil || enc.handle == 0 {
		return nil, &WGPUError{Op: "BeginRenderPass", Message: "encoder is nil or released"}
	}
	if desc == nil {
		return nil, &WGPUError{Op: "BeginRenderPass", Message: "descriptor is nil"}
	}
	if len(desc.ColorAttachments) == 0 {
		return nil, &WGPUError{Op: "BeginRenderPass", Message: "no color attachments"}
	}
	if err := enc.beginPass("BeginRenderPass"); err != nil {
		return nil, err
	}

	// Build native color attachments
	nativeColorAttachments := make([]renderPassColorAttachment, len(desc.ColorAttachments))
	for i, ca := range desc.ColorAttachments {
		var viewHandle uintptr
		if ca.View != nil {
			viewHandle = ca.View.handle
		}

		var resolveHandle uintptr
		if ca.ResolveTarget != nil {
			resolveHandle = ca.ResolveTarget.handle
		}

		nativeColorAttachments[i] = renderPassColorAttachment{
			nextInChain:   0,
			view:          viewHandle,
			depthSlice:    DepthSliceUndefined, // CRITICAL for 2D textures!
			resolveTarget: resolveHandle,
			loadOp:        uint32(ca.LoadOp),
			storeOp:       uint32(ca.StoreOp),
			clearValue:    ca.ClearValue,
		}
	}

	// Build depth/stencil attachment if present
	var depthStencilPtr uintptr
	var nativeDepthStencil renderPassDepthStencilAttachment
	if desc.DepthStencilAttachment != nil {
		depthRO := False
		if desc.DepthStencilAttachment.DepthReadOnly {
			depthRO = True
		}
		stencilRO := False
		if desc.DepthStencilAttachment.StencilReadOnly {
			stencilRO = True
		}

		nativeDepthStencil = renderPassDepthStencilAttachment{
			view:              desc.DepthStencilAttachment.View.handle,
			depthLoadOp:       uint32(desc.DepthStencilAttachment.DepthLoadOp),
			depthStoreOp:      uint32(desc.DepthStencilAttachment.DepthStoreOp),
			depthClearValue:   desc.DepthStencilAttachment.DepthClearValue,
			depthReadOnly:     depthRO,
			stencilLoadOp:     uint32(desc.DepthStencilAttachment.StencilLoadOp),
			stencilStoreOp:    uint32(desc.DepthStencilAttachment.StencilStoreOp),
			stencilClearValue: desc.DepthStencilAttachment.StencilClearValue,
			stencilReadOnly:   stencilRO,
		}
		depthStencilPtr = uintptr(unsafe.Pointer(&nativeDepthStencil))
	}

	// Build timestamp writes if present (v29: passTimestampWrites with nextInChain)
	var timestampWritesPtr uintptr
	var nativeTimestampWrites passTimestampWrites
	if desc.TimestampWrites != nil {
		nativeTimestampWrites = passTimestampWrites{
			nextInChain:               0,
			querySet:                  desc.TimestampWrites.QuerySet.handle,
			beginningOfPassWriteIndex: desc.TimestampWrites.BeginningOfPassWriteIndex,
			endOfPassWriteIndex:       desc.TimestampWrites.EndOfPassWriteIndex,
		}
		timestampWritesPtr = uintptr(unsafe.Pointer(&nativeTimestampWrites))
	}

	nativeDesc := renderPassDescriptor{
		nextInChain:            0,
		label:                  stringToStringView(desc.Label),
		colorAttachmentCount:   uintptr(len(nativeColorAttachments)),
		colorAttachments:       uintptr(unsafe.Pointer(&nativeColorAttachments[0])),
		depthStencilAttachment: depthStencilPtr,
		occlusionQuerySet:      0,
		timestampWrites:        timestampWritesPtr,
	}

	handle, _, _ := procCommandEncoderBeginRenderPass.Call(
		enc.handle,
		uintptr(unsafe.Pointer(&nativeDesc)),
	)
	if handle == 0 {
		enc.abortPass()
		return nil, &WGPUError{Op: "BeginRenderPass", Message: "wgpu returned null handle"}
	}
	trackResource(handle, "RenderPassEncoder")
	return &RenderPassEncoder{handle: handle, encoder: enc}, nil
}

// SetPipeline sets the render pipeline for this pass.
// Returns an error if the pass has ended or been released, or if the
// pipeline is nil or released.
func (rpe *RenderPassEncoder) SetPipeline(pipeline *RenderPipeline) error {
	mustInit()
	const op = "RenderPassEncoder.SetPipeline"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	if pipeline == nil || pipeline.handle == 0 {
		return lifecycleError(op, ErrDependencyReleased)
	}
	procRenderPassEncoderSetPipeline.Call(rpe.handle, pipeline.handle) //nolint:errcheck
	rpe.pipeline = pipeline
	return nil
}

// SetBindGroup sets a bind group for this pass.
func (rpe *RenderPassEncoder) SetBindGroup(groupIndex uint32, group *BindGroup, dynamicOffsets []uint32) error {
	mustInit()
	const op = "RenderPassEncoder.SetBindGroup"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	if group == nil || group.handle == 0 {
		return lifecycleError(op, ErrDependencyReleased)
	}

	var offsetsPtr uintptr
	offsetCount := uintptr(0)
	if len(dynamicOffsets) > 0 {
		offsetsPtr = uintptr(unsafe.Pointer(&dynamicOffsets[0]))
		offsetCount = uintptr(len(dynamicOffsets))
	}

	procRenderPassEncoderSetBindGroup.Call( //nolint:errcheck
		rpe.handle,
		uintptr(groupIndex),
		group.handle,
		offsetCount,
		offsetsPtr,
	)
	if rpe.bindGroups == nil {
		rpe.bindGroups = make(map[uint32]*BindGroup)
	}
	rpe.bindGroups[groupIndex] = group
	return nil
}

// SetVertexBuffer sets a vertex buffer for this pass.
func (rpe *RenderPassEncoder) SetVertexBuffer(slot uint32, buffer *Buffer, offset, size uint64) error {
	mustInit()
	const op = "RenderPassEncoder.SetVertexBuffer"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	if buffer == nil || buffer.handle == 0 {
		return lifecycleError(op, ErrDependencyReleased)
	}
	procRenderPassEncoderSetVertexBuffer.Call( //nolint:errcheck
		rpe.handle,
		uintptr(slot),
		buffer.handle,
		uintptr(offset),
		uintptr(size),
	)
	if rpe.vertexBuffers == nil {
		rpe.vertexBuffers = make(map[uint32]*Buffer)
	}
	rpe.vertexBuffers[slot] = buffer
	return nil
}

// SetIndexBuffer sets the index buffer for this pass.
func (rpe *RenderPassEncoder) SetIndexBuffer(buffer *Buffer, format gputypes.IndexFormat, offset, size uint64) error {
	mustInit()
	const op = "RenderPassEncoder.SetIndexBuffer"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	if buffer == nil || buffer.handle == 0 {
		return lifecycleError(op, ErrDependencyReleased)
	}
	procRenderPassEncoderSetIndexBuffer.Call( //nolint:errcheck
		rpe.handle,
		buffer.handle,
		uintptr(format),
		uintptr(offset),
		uintptr(size),
	)
	rpe.indexBuffer = buffer
	return nil
}

// Draw draws primitives.
// Fails if the pass has ended or a resource bound to the pass (pipeline,
// vertex/index buffer, bind group) has been released.
func (rpe *RenderPassEncoder) Draw(vertexCount, instanceCount, firstVertex, firstInstance uint32) error {
	mustInit()
	const op = "RenderPassEncoder.Draw"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	if err := rpe.checkDependenciesLocked(op); err != nil {
		return err
	}
	procRenderPassEncoderDraw.Call( //nolint:errcheck
		rpe.handle,
		uintptr(vertexCount),
		uintptr(instanceCount),
		uintptr(firstVertex),
		uintptr(firstInstance),
	)
	return nil
}

// DrawIndexed draws indexed primitives.
func (rpe *RenderPassEncoder) DrawIndexed(indexCount, instanceCount, firstIndex uint32, baseVertex int32, firstInstance uint32) error {
	mustInit()
	const op = "RenderPassEncoder.DrawIndexed"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	if err := rpe.checkDependenciesLocked(op); err != nil {
		return err
	}
	procRenderPassEncoderDrawIndexed.Call( //nolint:errcheck
		rpe.handle,
		uintptr(indexCount),
		uintptr(instanceCount),
		uintptr(firstIndex),
		uintptr(baseVertex),
		uintptr(firstInstance),
	)
	return nil
}

// DrawIndirect draws primitives using parameters from a GPU buffer.
// indirectBuffer must contain a DrawIndirectArgs structure:
//   - vertexCount (uint32)
//   - instanceCount (uint32)
//   - firstVertex (uint32)
//   - firstInstance (uint32)
func (rpe *RenderPassEncoder) DrawIndirect(indirectBuffer *Buffer, indirectOffset uint64) error {
	mustInit()
	const op = "RenderPassEncoder.DrawIndirect"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	if err := rpe.checkDependenciesLocked(op); err != nil {
		return err
	}
	if indirectBuffer == nil || indirectBuffer.handle == 0 {
		return lifecycleError(op, ErrDependencyReleased)
	}
	procRenderPassEncoderDrawIndirect.Call( //nolint:errcheck
		rpe.handle,
		indirectBuffer.handle,
		uintptr(indirectOffset),
	)
	return nil
}

// DrawIndexedIndirect draws indexed primitives using parameters from a GPU buffer.
// indirectBuffer must contain a DrawIndexedIndirectArgs structure:
//   - indexCount (uint32)
//   - instanceCount (uint32)
//   - firstIndex (uint32)
//   - baseVertex (int32)
//   - firstInstance (uint32)
func (rpe *RenderPassEncoder) DrawIndexedIndirect(indirectBuffer *Buffer, indirectOffset uint64) error {
	mustInit()
	const op = "RenderPassEncoder.DrawIndexedIndirect"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	if err := rpe.checkDependenciesLocked(op); err != nil {
		return err
	}
	if indirectBuffer == nil || indirectBuffer.handle == 0 {
		return lifecycleError(op, ErrDependencyReleased)
	}
	procRenderPassEncoderDrawIndexedIndirect.Call( //nolint:errcheck
		rpe.handle,
		indirectBuffer.handle,
		uintptr(indirectOffset),
	)
	return nil
}

// SetViewport sets the viewport used during the rasterization stage.
// x, y: top-left corner of the viewport in pixels
// width, height: dimensions of the viewport in pixels
// minDepth, maxDepth: depth range for the viewport (typically 0.0 to 1.0)
func (rpe *RenderPassEncoder) SetViewport(x, y, width, height, minDepth, maxDepth float32) error {
	mustInit()
	const op = "RenderPassEncoder.SetViewport"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	procRenderPassEncoderSetViewport.Call( //nolint:errcheck
		rpe.handle,
		uintptr(math.Float32bits(x)),
		uintptr(math.Float32bits(y)),
		uintptr(math.Float32bits(width)),
		uintptr(math.Float32bits(height)),
		uintptr(math.Float32bits(minDepth)),
		uintptr(math.Float32bits(maxDepth)),
	)
	return nil
}

// SetScissorRect sets the scissor rectangle used during the rasterization stage.
// Pixels outside the scissor rectangle will be discarded.
// x, y: top-left corner of the scissor rectangle in pixels
// width, height: dimensions of the scissor rectangle in pixels
func (rpe *RenderPassEncoder) SetScissorRect(x, y, width, height uint32) error {
	mustInit()
	const op = "RenderPassEncoder.SetScissorRect"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	procRenderPassEncoderSetScissorRect.Call( //nolint:errcheck
		rpe.handle,
		uintptr(x),
		uintptr(y),
		uintptr(width),
		uintptr(height),
	)
	return nil
}

// SetBlendConstant sets the blend constant color used by blend operations.
// Errors are reported via Device error scopes.
func (rpe *RenderPassEncoder) SetBlendConstant(color *Color) error {
	mustInit()
	const op = "RenderPassEncoder.SetBlendConstant"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	if color == nil {
		return &WGPUError{Op: op, Message: "color is nil"}
	}
	procRenderPassEncoderSetBlendConstant.Call( //nolint:errcheck
		rpe.handle,
		uintptr(unsafe.Pointer(color)),
	)
	return nil
}

// SetStencilReference sets the stencil reference value used by stencil operations.
func (rpe *RenderPassEncoder) SetStencilReference(reference uint32) error {
	mustInit()
	const op = "RenderPassEncoder.SetStencilReference"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	procRenderPassEncoderSetStencilReference.Call( //nolint:errcheck
		rpe.handle,
		uintptr(reference),
	)
	return nil
}

// InsertDebugMarker inserts a single debug marker label into the render pass.
// This is useful for GPU debugging tools to identify specific command points.
func (rpe *RenderPassEncoder) InsertDebugMarker(markerLabel string) error {
	mustInit()
	const op = "RenderPassEncoder.InsertDebugMarker"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	labelBytes := []byte(markerLabel)
	if len(labelBytes) == 0 {
		return nil
	}
	label := StringView{
		Data:   uintptr(unsafe.Pointer(&labelBytes[0])),
		Length: uintptr(len(labelBytes)),
	}
	procRenderPassEncoderInsertDebugMarker.Call( //nolint:errcheck
		rpe.handle,
		uintptr(unsafe.Pointer(&label)),
	)
	return nil
}

// PushDebugGroup begins a labeled debug group in the render pass.
// Use PopDebugGroup to end the group. Groups can be nested.
func (rpe *RenderPassEncoder) PushDebugGroup(groupLabel string) error {
	mustInit()
	const op = "RenderPassEncoder.PushDebugGroup"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	labelBytes := []byte(groupLabel)
	if len(labelBytes) == 0 {
		return nil
	}
	label := StringView{
		Data:   uintptr(unsafe.Pointer(&labelBytes[0])),
		Length: uintptr(len(labelBytes)),
	}
	procRenderPassEncoderPushDebugGroup.Call( //nolint:errcheck
		rpe.handle,
		uintptr(unsafe.Pointer(&label)),
	)
	return nil
}

// PopDebugGroup ends the current debug group in the render pass.
// Must match a preceding PushDebugGroup call.
func (rpe *RenderPassEncoder) PopDebugGroup() error {
	mustInit()
	const op = "RenderPassEncoder.PopDebugGroup"
	if err := rpe.lockRecording(op); err != nil {
		return err
	}
	defer rpe.mu.Unlock()
	procRenderPassEncoderPopDebugGroup.Call(rpe.handle) //nolint:errcheck
	return nil
}

// End ends the render pass.
// The first call ends the pass in the native layer; subsequent calls return
// ErrPassEnded without touching the native layer again. After End, all
// recording methods on this pass are rejected.
func (rpe *RenderPassEncoder) End() error {
	mustInit()
	const op = "RenderPassEncoder.End"
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
	rpe.ended = true
	handle := rpe.handle
	enc := rpe.encoder
	rpe.mu.Unlock()

	if handle != 0 {
		procRenderPassEncoderEnd.Call(handle) //nolint:errcheck
	}
	if enc != nil {
		enc.passEnded()
	}
	return nil
}

// Release releases the render pass encoder.
// It is idempotent: the native release runs at most once. Releasing a pass
// that was never ended invalidates its parent command encoder.
func (rpe *RenderPassEncoder) Release() {
	if rpe == nil {
		return
	}
	rpe.mu.Lock()
	if rpe.released {
		rpe.mu.Unlock()
		return
	}
	rpe.released = true
	wasEnded := rpe.ended
	handle := rpe.handle
	rpe.handle = 0
	enc := rpe.encoder
	rpe.encoder = nil
	rpe.pipeline = nil
	rpe.vertexBuffers = nil
	rpe.indexBuffer = nil
	rpe.bindGroups = nil
	rpe.mu.Unlock()

	if handle != 0 {
		untrackResource(handle)
		procRenderPassEncoderRelease.Call(handle) //nolint:errcheck
	}
	if enc != nil && !wasEnded {
		enc.passAbandoned()
	}
}

// Handle returns the underlying handle. For advanced use only.
func (rpe *RenderPassEncoder) Handle() uintptr { return rpe.handle }
