package roc

import "errors"

// Stable errors returned once an object has reached its one-shot terminal
// (closed) state. They are package-level values so callers can use errors.Is
// instead of matching on error text.
var (
	// errContextClosed is returned by every Context entry point after the
	// context has been closed successfully.
	errContextClosed = errors.New("context is closed")

	// errSenderClosed is returned by every Sender entry point after the
	// sender has been closed successfully.
	errSenderClosed = errors.New("sender is closed")

	// errReceiverClosed is returned by every Receiver entry point after the
	// receiver has been closed successfully.
	errReceiverClosed = errors.New("receiver is closed")
)

// slotBook keeps Go-side knowledge of the slots that have been marked broken
// by a failed configure / connect / bind on a peer.
//
// The native library owns the authoritative slot state; this mirror exists so
// the bindings can enforce the contract documented on the peer methods: a
// broken slot stays reserved and unusable until Unlink releases it, after
// which the index is clean and can be reused with no leftover state.
//
// All access happens while the owning peer's lock is held.
type slotBook struct {
	// broken holds every slot currently disabled by a failed operation. The
	// slot index stays reserved (and further calls are rejected) until Unlink
	// removes it from this set.
	broken map[Slot]struct{}
}

func newSlotBook() *slotBook {
	return &slotBook{broken: make(map[Slot]struct{})}
}

// markBroken records that a slot has been disabled by a failed native
// configure / connect / bind call.
func (b *slotBook) markBroken(slot Slot) {
	b.broken[slot] = struct{}{}
}

// isBroken reports whether the slot is currently reserved as broken.
func (b *slotBook) isBroken(slot Slot) bool {
	_, ok := b.broken[slot]
	return ok
}

// release forgets a slot after a successful Unlink, clearing its broken marker
// so that the index can be reused from a clean state.
func (b *slotBook) release(slot Slot) {
	delete(b.broken, slot)
}
