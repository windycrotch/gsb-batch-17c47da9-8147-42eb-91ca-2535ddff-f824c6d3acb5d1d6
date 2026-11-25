package roc

/*
#include <roc/context.h>
*/
import "C"

import (
	"sync"
)

// Shared context.
//
// Context contains memory pools and network worker threads, shared among
// objects attached to the context. It is allowed both to create a separate
// context for every object, or to create a single context shared between
// multiple objects.
//
// # Life cycle
//
// A context is created using OpenContext() and destroyed using Context.Close().
// Objects can be attached and detached to an opened context at any moment from
// any thread. However, the user should ensure that the context is not closed
// until there are no objects attached to the context.
//
// # Thread safety
//
// # Can be used concurrently
//
// # See also
//
// See also Sender, Receiver
type Context struct {
	mu sync.RWMutex

	cPtr *C.roc_context

	// closed is the authoritative one-shot terminal flag. It is set only
	// after the native context has been closed successfully. All entry
	// points check it under mu so they never touch a freed native pointer.
	closed bool
}

// Open a new context.
//
// Allocates and initializes a new context. May start some background threads.
// Overrides the provided Result pointer with the newly created context.
func OpenContext(config ContextConfig) (ctx *Context, err error) {
	logWrite(LogDebug, "entering OpenContext(): config=%+v", config)
	defer func() {
		logWrite(LogDebug, "leaving OpenContext(): context=%p err=%#v", ctx, err)
	}()

	checkVersionFn()

	cConfig := C.struct_roc_context_config{
		max_packet_size: C.uint(config.MaxPacketSize),
		max_frame_size:  C.uint(config.MaxFrameSize),
	}

	var cCtx *C.roc_context
	errCode := C.roc_context_open(&cConfig, &cCtx)
	if errCode != 0 {
		return nil, newNativeErr("roc_context_open()", errCode)
	}
	if cCtx == nil {
		panic("roc_context_open() returned nil")
	}

	ctx = &Context{
		cPtr: cCtx,
	}

	return ctx, nil
}

// Register custom encoding.
//
// Registers Encoding with given EncodingId. Registered encodings complement
// built-in encodings defined by PacketEncoding enum. Whenever you need to
// specify packet encoding, you can use both built-in and registered encodings.
//
// On sender, you should register custom encoding and set to PacketEncoding
// field of SenderConfig, if you need to force specific encoding of packets, but
// built-in set of encodings is not enough.
//
// On receiver, you should register custom encoding with same id and
// specification, if you did so on sender, and you're not using any signaling
// protocol (like RTSP) that is capable of automatic exchange of encoding
// information.
//
// In case of RTP, encoding id is mapped directly to payload type field (PT).
func (c *Context) RegisterEncoding(encodingID int, encoding MediaEncoding) (err error) {
	logWrite(LogDebug,
		"entering Context.RegisterEncoding(): context=%p id=%+v encoding=%+v",
		c, encodingID, encoding,
	)
	defer func() {
		logWrite(LogDebug, "leaving Context.RegisterEncoding(): context=%p err=%#v", c, err)
	}()

	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.closed || c.cPtr == nil {
		return errContextClosed
	}

	cEncoding := C.struct_roc_media_encoding{
		rate:     C.uint(encoding.Rate),
		format:   C.roc_format(encoding.Format),
		channels: C.roc_channel_layout(encoding.Channels),
		tracks:   C.uint(encoding.Tracks),
	}

	var errCode C.int

	errCode = C.roc_context_register_encoding(
		c.cPtr,
		C.int(encodingID),
		&cEncoding)
	if errCode != 0 {
		return newNativeErr("roc_context_register_encoding()", errCode)
	}

	return nil
}

// Close the context.
//
// Stops any started background threads, deinitializes and deallocates the
// context. The user should ensure that nobody uses the context during and after
// this call.
//
// If this function fails, the context is kept opened.
func (c *Context) Close() (err error) {
	logWrite(LogDebug, "entering Context.Close(): context=%p", c)
	defer func() {
		logWrite(LogDebug, "leaving Context.Close(): context=%p err=%#v", c, err)
	}()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Idempotent: closing an already closed context is a successful no-op so
	// that Close is a stable, one-shot terminal operation.
	if c.closed || c.cPtr == nil {
		return nil
	}

	errCode := C.roc_context_close(c.cPtr)
	if errCode != 0 {
		// On failure the native context stays open and the closed flag stays
		// clear, so the object remains usable and Close can be retried.
		return newNativeErr("roc_context_close()", errCode)
	}

	c.cPtr = nil
	c.closed = true

	return nil
}

// beginOpen is shared by OpenSender and OpenReceiver. It holds the context
// read lock for the whole duration of the native open call and refuses to run
// once the context has been closed. Holding the lock means a concurrent
// Context.Close cannot free the native context while an open is in flight.
// The returned release function must be invoked exactly once when the open
// attempt finishes.
func (c *Context) beginOpen() (release func(), err error) {
	c.mu.RLock()

	if c.closed || c.cPtr == nil {
		c.mu.RUnlock()
		return nil, errContextClosed
	}

	return c.mu.RUnlock, nil
}
