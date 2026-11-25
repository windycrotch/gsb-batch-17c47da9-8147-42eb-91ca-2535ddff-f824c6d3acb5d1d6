package roc

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This suite exercises the cross-goroutine lifecycle contract shared by
// Context, Sender and Receiver using real OpenContext/OpenSender/OpenReceiver
// calls and local loopback endpoints:
//
//   - normal send/receive still works;
//   - unlink and close interleaved with frame I/O never race or crash;
//   - a failed Close leaves the object usable and retryable;
//   - a slot broken by a failed operation can be unlinked and its index
//     reused, with no leftover state;
//   - once Close succeeds, every entry point stably reports "closed";
//   - a closed context rejects new peers while closing already-attached peers
//     never panics.
//
// All network endpoints use the plain rtp:// scheme (FEC disabled) so the
// tests do not depend on optional FEC codecs being present in the native build.

const (
	loopbackRTP = "rtp://127.0.0.1:0"
	// hammerDuration bounds the concurrent stress tests so they are
	// deterministic while still interleaving many operations.
	hammerDuration = 300 * time.Millisecond
	// churnInterval paces slot create/teardown cycles. It is deliberately
	// slower than the native teardown so a bind/unlink pair fully completes
	// before the next one starts, keeping the stress test deterministic
	// while still interleaving slot operations with frame I/O and close.
	churnInterval = 20 * time.Millisecond
)

// openTestContext returns a fresh context that is closed on test cleanup.
func openTestContext(t *testing.T) *Context {
	t.Helper()
	ctx, err := OpenContext(makeContextConfig())
	require.NoError(t, err)
	require.NotNil(t, ctx)
	t.Cleanup(func() { _ = ctx.Close() })
	return ctx
}

// openTestReceiver returns a receiver attached to ctx, closed on cleanup.
func openTestReceiver(t *testing.T, ctx *Context) *Receiver {
	t.Helper()
	recv, err := OpenReceiver(ctx, makeReceiverConfig())
	require.NoError(t, err)
	require.NotNil(t, recv)
	t.Cleanup(func() { _ = recv.Close() })
	return recv
}

// openTestSender returns a sender attached to ctx, closed on cleanup. The
// sender uses FEC disabled and the external clock so it works over a plain
// rtp:// endpoint without depending on optional native FEC codecs, and never
// blocks inside WriteFloats.
func openTestSender(t *testing.T, ctx *Context) *Sender {
	t.Helper()
	sender, err := OpenSender(ctx, disabledSenderConfig())
	require.NoError(t, err)
	require.NotNil(t, sender)
	t.Cleanup(func() { _ = sender.Close() })
	return sender
}

// loopbackPair binds a receiver on a fresh loopback endpoint and connects a
// sender to it, returning the running pair plus the bound endpoint.
func loopbackPair(t *testing.T, ctx *Context) (*Sender, *Receiver, *Endpoint) {
	t.Helper()
	recv := openTestReceiver(t, ctx)
	sender := openTestSender(t, ctx)

	endpoint, err := ParseEndpoint(loopbackRTP)
	require.NoError(t, err)

	require.NoError(t, recv.Bind(SlotDefault, InterfaceAudioSource, endpoint))
	require.NotEmpty(t, endpoint.Port)

	require.NoError(t, sender.Connect(SlotDefault, InterfaceAudioSource, endpoint))
	return sender, recv, endpoint
}

func testFrame() []float32 {
	frame := make([]float32, NumChannels)
	frame[0] = 0.25
	frame[1] = -0.25
	return frame
}

// waitForNonZero reads from the receiver until non-zero samples arrive or the
// deadline expires. It proves that audio written by the sender really reaches
// the receiver over loopback.
func waitForNonZero(t *testing.T, recv *Receiver, frameSize int) {
	t.Helper()
	buf := make([]float32, frameSize)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, recv.ReadFloats(buf))
		for _, v := range buf {
			if v != 0 {
				return
			}
		}
	}
	t.Fatal("timed out waiting for non-zero samples from receiver")
}

// pumpFrames writes frames to the sender in a tight loop until stop is closed,
// keeping the sender's packet buffer fed so the receiver keeps producing
// samples. The returned WaitGroup is done once pumping has stopped.
func pumpFrames(sender *Sender, frame []float32, stop <-chan struct{}) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if err := sender.WriteFloats(frame); err != nil {
					return
				}
			}
		}
	}()
	return &wg
}

// assertAudioFlows pumps the sender and reads the receiver until real samples
// arrive, proving the pair carries audio over loopback. It stops both loops
// before returning.
func assertAudioFlows(t *testing.T, sender *Sender, recv *Receiver) {
	t.Helper()
	frame := testFrame()
	stop := make(chan struct{})
	pumpWG := pumpFrames(sender, frame, stop)
	waitForNonZero(t, recv, len(frame))
	close(stop)
	pumpWG.Wait()
}

// TestLifecycle_NormalSendReceive guards the happy path: with the new locking
// in place, a real loopback stream still flows end to end.
func TestLifecycle_NormalSendReceive(t *testing.T) {
	ctx := openTestContext(t)
	sender, recv, _ := loopbackPair(t, ctx)
	assertAudioFlows(t, sender, recv)
}

// TestLifecycle_ConcurrentUnlinkAndClose_Receiver interleaves continuous
// reads, repeated unlink/bind churn on a secondary slot, and a final close.
// After close returns, every entry point must report the stable closed error.
func TestLifecycle_ConcurrentUnlinkAndClose_Receiver(t *testing.T) {
	ctx := openTestContext(t)
	sender, recv, _ := loopbackPair(t, ctx)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Continuous frame reads.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]float32, NumChannels)
		for {
			select {
			case <-stop:
				return
			default:
				_ = recv.ReadFloats(buf)
			}
		}
	}()

	// Keep the sender producing so reads have a live stream to consume.
	wg.Add(1)
	go func() {
		defer wg.Done()
		frame := testFrame()
		for {
			select {
			case <-stop:
				return
			default:
				_ = sender.WriteFloats(frame)
			}
		}
	}()

	// Repeatedly bind and unlink a secondary slot to exercise the slot
	// bookkeeping concurrently with frame I/O. The loop is paced by a ticker
	// (rather than spinning) so each native create/teardown pair completes
	// before the next begins.
	var churnWG sync.WaitGroup
	churnDone := make(chan struct{})
	churnWG.Add(1)
	go func() {
		defer churnWG.Done()
		slot := Slot(3)
		ticker := time.NewTicker(churnInterval)
		defer ticker.Stop()
		for {
			select {
			case <-churnDone:
				return
			case <-ticker.C:
				if ep, err := ParseEndpoint(loopbackRTP); err == nil {
					// Best-effort bind; an error marks the slot broken, which
					// the following Unlink must be able to release.
					_ = recv.Bind(slot, InterfaceAudioSource, ep)
				}
				_ = recv.Unlink(slot)
			}
		}
	}()

	time.Sleep(hammerDuration)
	// Join the slot churn before Close so two native teardown paths (unlink
	// and close) never overlap; frame I/O below is still racing Close.
	close(churnDone)
	churnWG.Wait()

	// Close while the I/O goroutines are still calling in. Close must wait
	// for in-flight operations and then become the one-shot terminal state.
	require.NoError(t, recv.Close())
	close(stop)
	wg.Wait()

	// Every entry point now stably reports the closed terminal state.
	ep, err := ParseEndpoint(loopbackRTP)
	require.NoError(t, err)
	require.True(t, errors.Is(recv.Bind(SlotDefault, InterfaceAudioSource, ep), errReceiverClosed))
	require.True(t, errors.Is(recv.Configure(SlotDefault, InterfaceAudioSource, makeInterfaceConfig()), errReceiverClosed))
	require.True(t, errors.Is(recv.Unlink(SlotDefault), errReceiverClosed))
	require.True(t, errors.Is(recv.ReadFloats(make([]float32, NumChannels)), errReceiverClosed))

	// Close remains idempotent and successful.
	require.NoError(t, recv.Close())
}

// TestLifecycle_ConcurrentUnlinkAndClose_Sender mirrors the receiver case for
// the sender side: continuous writes and slot churn racing with Close.
func TestLifecycle_ConcurrentUnlinkAndClose_Sender(t *testing.T) {
	ctx := openTestContext(t)
	sender, recv, _ := loopbackPair(t, ctx)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		frame := testFrame()
		for {
			select {
			case <-stop:
				return
			default:
				_ = sender.WriteFloats(frame)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]float32, NumChannels)
		for {
			select {
			case <-stop:
				return
			default:
				_ = recv.ReadFloats(buf)
			}
		}
	}()

	var churnWG sync.WaitGroup
	churnDone := make(chan struct{})
	churnWG.Add(1)
	go func() {
		defer churnWG.Done()
		slot := Slot(5)
		// A protocol/interface mismatch makes connect fail and break a real
		// native slot, which Unlink then releases; the churn thus exercises
		// both the broken and the released states. Paced by a ticker so each
		// native create/teardown pair completes before the next begins.
		ep, parseErr := ParseEndpoint(loopbackRTP)
		if parseErr != nil {
			return
		}
		ticker := time.NewTicker(churnInterval)
		defer ticker.Stop()
		for {
			select {
			case <-churnDone:
				return
			case <-ticker.C:
				_ = sender.Connect(slot, InterfaceAudioRepair, ep)
				_ = sender.Unlink(slot)
			}
		}
	}()

	time.Sleep(hammerDuration)
	// Join churn before Close so native unlink and close teardowns never
	// overlap; frame I/O above is still racing Close.
	close(churnDone)
	churnWG.Wait()

	require.NoError(t, sender.Close())
	close(stop)
	wg.Wait()

	require.True(t, errors.Is(sender.Configure(SlotDefault, InterfaceAudioSource, makeInterfaceConfig()), errSenderClosed))
	require.True(t, errors.Is(sender.Connect(SlotDefault, InterfaceAudioSource, nil), errSenderClosed))
	require.True(t, errors.Is(sender.Unlink(SlotDefault), errSenderClosed))
	require.True(t, errors.Is(sender.WriteFloats(testFrame()), errSenderClosed))
	require.NoError(t, sender.Close())
}

// TestLifecycle_CloseFailureKeepsObjectUsable uses the native reference
// counting rule: a Context.Close fails while a peer is still attached. The
// failed close must leave the context fully usable; after detaching the peer,
// retrying Close must succeed.
func TestLifecycle_CloseFailureKeepsObjectUsable(t *testing.T) {
	ctx := openTestContext(t)
	recv := openTestReceiver(t, ctx)
	sender := openTestSender(t, ctx)

	// First attempt fails because peers are still attached.
	err := ctx.Close()
	var native nativeErr
	require.ErrorAs(t, err, &native)
	require.Equal(t, "roc_context_close()", native.op)

	// Context is still usable: it can still answer entry points and mint
	// new peers.
	require.NoError(t, ctx.RegisterEncoding(60, makeMediaEncoding()))
	extra, err := OpenReceiver(ctx, makeReceiverConfig())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.NoError(t, extra.Close())

	// Detach the originally attached peers; now Close succeeds on retry.
	require.NoError(t, recv.Close())
	require.NoError(t, sender.Close())
	require.NoError(t, ctx.Close())

	// After success it is terminal and a further Close is an idempotent no-op.
	require.NoError(t, ctx.Close())
	_, err = OpenSender(ctx, disabledSenderConfig())
	require.True(t, errors.Is(err, errContextClosed))
}

// disabledSenderConfig returns a sender configuration with FEC turned off so
// it operates over plain rtp:// endpoints regardless of the native build's
// available FEC codecs.
func disabledSenderConfig() SenderConfig {
	cfg := makeSenderConfig()
	cfg.FecEncoding = FecEncodingDisable
	cfg.ClockSource = ClockSourceExternal
	return cfg
}

// TestLifecycle_BrokenSlotReleaseAndReuse_Receiver drives a receiver slot
// through the broken -> released -> reused lifecycle and proves no state
// leaks between the two uses of the same index.
func TestLifecycle_BrokenSlotReleaseAndReuse_Receiver(t *testing.T) {
	ctx := openTestContext(t)
	recv := openTestReceiver(t, ctx)

	// First establish a working slot 0 bound to a concrete loopback port.
	firstEndp, err := ParseEndpoint(loopbackRTP)
	require.NoError(t, err)
	require.NoError(t, recv.Bind(SlotDefault, InterfaceAudioSource, firstEndp))
	require.NotEmpty(t, firstEndp.Port)

	// Binding a second slot to the already-used local address fails and marks
	// that second slot broken. This is independent of optional native FEC
	// codecs.
	slot := Slot(4)
	dupEndp := &Endpoint{
		Protocol: firstEndp.Protocol,
		Host:     firstEndp.Host,
		Port:     firstEndp.Port,
	}
	bindErr := recv.Bind(slot, InterfaceAudioSource, dupEndp)
	require.Error(t, bindErr)
	require.True(t, recv.slots.isBroken(slot))

	// While broken, even a valid operation on that index must be rejected.
	goodEndp, err := ParseEndpoint(loopbackRTP)
	require.NoError(t, err)
	require.Error(t, recv.Bind(slot, InterfaceAudioSource, goodEndp))

	// Unlink releases the slot and clears the Go-side marker.
	require.NoError(t, recv.Unlink(slot))
	require.False(t, recv.slots.isBroken(slot))

	// The same index is now reusable from a clean state and actually serves
	// traffic, proving there is no leftover interface state.
	reusedEndp, err := ParseEndpoint(loopbackRTP)
	require.NoError(t, err)
	require.NoError(t, recv.Bind(slot, InterfaceAudioSource, reusedEndp))
	require.NotEmpty(t, reusedEndp.Port)

	sender := openTestSender(t, ctx)
	require.NoError(t, sender.Connect(SlotDefault, InterfaceAudioSource, reusedEndp))

	// Reused slot actually carries traffic, proving no interface state leaked.
	assertAudioFlows(t, sender, recv)
}

// TestLifecycle_BrokenSlotReleaseAndReuse_Sender is the sender counterpart:
// a failed connect reserves the index, Unlink frees it, and a subsequent
// connect on the same index succeeds.
func TestLifecycle_BrokenSlotReleaseAndReuse_Sender(t *testing.T) {
	ctx := openTestContext(t)
	sender := openTestSender(t, ctx)
	recv := openTestReceiver(t, ctx)
	slot := Slot(2)

	// Bind the receiver so the eventual reuse connect has a live target.
	liveEndp, err := ParseEndpoint(loopbackRTP)
	require.NoError(t, err)
	require.NoError(t, recv.Bind(SlotDefault, InterfaceAudioSource, liveEndp))
	require.NotEmpty(t, liveEndp.Port)

	// Connecting the repair interface with an audio-source protocol is a
	// deterministic configuration error: the native library allocates the
	// slot, then fails and marks it broken. Because the slot really exists,
	// it can be released with Unlink and the index reused afterwards.
	err = sender.Connect(slot, InterfaceAudioRepair, liveEndp)
	require.Error(t, err)
	require.True(t, sender.slots.isBroken(slot))

	// The broken index rejects further operations until unlinked.
	require.Error(t, sender.Connect(slot, InterfaceAudioSource, liveEndp))

	require.NoError(t, sender.Unlink(slot))
	require.False(t, sender.slots.isBroken(slot))

	// Reusing the freed index now succeeds against the live receiver.
	require.NoError(t, sender.Connect(slot, InterfaceAudioSource, liveEndp))

	// The reused sender slot carries real audio to the receiver.
	assertAudioFlows(t, sender, recv)
}

// TestLifecycle_ClosedContextRejectsNewPeers guarantees that once a context
// is closed, opening either kind of peer fails with the stable closed error
// instead of touching the dead native context.
func TestLifecycle_ClosedContextRejectsNewPeers(t *testing.T) {
	ctx := openTestContext(t)
	require.NoError(t, ctx.Close())

	sender, err := OpenSender(ctx, disabledSenderConfig())
	require.Nil(t, sender)
	require.True(t, errors.Is(err, errContextClosed))

	recv, err := OpenReceiver(ctx, makeReceiverConfig())
	require.Nil(t, recv)
	require.True(t, errors.Is(err, errContextClosed))

	require.True(t, errors.Is(ctx.RegisterEncoding(61, makeMediaEncoding()), errContextClosed))
}

// TestLifecycle_ClosingPeerAfterContextClosedDoesNotPanic closes a context
// while peers are attached (the context close fails), then closes the peers
// afterwards. Peer close must complete cleanly and never panic even though
// the context is in the "failed close, still open" state.
func TestLifecycle_ClosingPeerAfterContextClosedDoesNotPanic(t *testing.T) {
	ctx := openTestContext(t)
	sender := openTestSender(t, ctx)
	recv := openTestReceiver(t, ctx)

	// This close is expected to fail while the peers are attached.
	require.Error(t, ctx.Close())

	// Closing the peers after the attempted context close must not panic and
	// must succeed. The native library detaches each peer from the context.
	require.NotPanics(t, func() {
		require.NoError(t, sender.Close())
		require.NoError(t, recv.Close())
	})

	// All peers are detached now, so the context can finally be closed.
	require.NoError(t, ctx.Close())
}

// TestLifecycle_AllEntriesRejectAfterClose runs a broad interleaving where a
// peer is hammered through every entry point from many goroutines and then
// closed; after close, repeated calls from all goroutines must only ever see
// the closed error and never a stray success.
func TestLifecycle_AllEntriesRejectAfterClose(t *testing.T) {
	t.Run("sender", func(t *testing.T) {
		ctx := openTestContext(t)
		sender, _, _ := loopbackPair(t, ctx)
		hammerSenderThenAssertClosed(t, sender)
	})
	t.Run("receiver", func(t *testing.T) {
		ctx := openTestContext(t)
		_, recv, _ := loopbackPair(t, ctx)
		hammerReceiverThenAssertClosed(t, recv)
	})
}

func hammerSenderThenAssertClosed(t *testing.T, sender *Sender) {
	stop := make(chan struct{})
	var wg sync.WaitGroup

	writers := 4
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frame := testFrame()
			for {
				select {
				case <-stop:
					return
				default:
					_ = sender.WriteFloats(frame)
				}
			}
		}()
	}
	// Slot churn using a legal lifecycle on a fixed secondary slot: connect
	// (which breaks the slot via a protocol/interface mismatch) followed by
	// unlink to release it. Paced so native teardown never overlaps the next
	// creation. It is joined before Close so unlink/close teardowns never race.
	var churnWG sync.WaitGroup
	stopChurn := make(chan struct{})
	churnWG.Add(1)
	go func() {
		defer churnWG.Done()
		ep, parseErr := ParseEndpoint(loopbackRTP)
		if parseErr != nil {
			return
		}
		slot := Slot(20)
		ticker := time.NewTicker(churnInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopChurn:
				return
			case <-ticker.C:
				_ = sender.Connect(slot, InterfaceAudioRepair, ep)
				_ = sender.Unlink(slot)
			}
		}
	}()

	time.Sleep(hammerDuration)
	close(stopChurn)
	churnWG.Wait()
	require.NoError(t, sender.Close())
	close(stop)
	wg.Wait()

	// A second wave after close: every result must be the closed error.
	var wg2 sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if err := sender.WriteFloats(testFrame()); !errors.Is(err, errSenderClosed) {
				t.Errorf("WriteFloats after close: got %v want %v", err, errSenderClosed)
			}
			if err := sender.Unlink(SlotDefault); !errors.Is(err, errSenderClosed) {
				t.Errorf("Unlink after close: got %v want %v", err, errSenderClosed)
			}
			if err := sender.Configure(SlotDefault, InterfaceAudioSource, makeInterfaceConfig()); !errors.Is(err, errSenderClosed) {
				t.Errorf("Configure after close: got %v want %v", err, errSenderClosed)
			}
		}()
	}
	wg2.Wait()
}

func hammerReceiverThenAssertClosed(t *testing.T, recv *Receiver) {
	stop := make(chan struct{})
	var wg sync.WaitGroup

	readers := 4
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]float32, NumChannels)
			for {
				select {
				case <-stop:
					return
				default:
					_ = recv.ReadFloats(buf)
				}
			}
		}()
	}
	// Slot churn using a legal lifecycle on a fixed secondary slot: bind
	// (which may break the slot on an address conflict) followed by unlink to
	// release it. Paced so native teardown never overlaps the next creation,
	// and joined before Close so unlink/close teardowns never race.
	var churnWG sync.WaitGroup
	stopChurn := make(chan struct{})
	churnWG.Add(1)
	go func() {
		defer churnWG.Done()
		slot := Slot(20)
		ticker := time.NewTicker(churnInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopChurn:
				return
			case <-ticker.C:
				if ep, parseErr := ParseEndpoint(loopbackRTP); parseErr == nil {
					_ = recv.Bind(slot, InterfaceAudioSource, ep)
				}
				_ = recv.Unlink(slot)
			}
		}
	}()

	time.Sleep(hammerDuration)
	close(stopChurn)
	churnWG.Wait()
	require.NoError(t, recv.Close())
	close(stop)
	wg.Wait()

	var wg2 sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if err := recv.ReadFloats(make([]float32, NumChannels)); !errors.Is(err, errReceiverClosed) {
				t.Errorf("ReadFloats after close: got %v want %v", err, errReceiverClosed)
			}
			if err := recv.Unlink(SlotDefault); !errors.Is(err, errReceiverClosed) {
				t.Errorf("Unlink after close: got %v want %v", err, errReceiverClosed)
			}
			if err := recv.Configure(SlotDefault, InterfaceAudioSource, makeInterfaceConfig()); !errors.Is(err, errReceiverClosed) {
				t.Errorf("Configure after close: got %v want %v", err, errReceiverClosed)
			}
		}()
	}
	wg2.Wait()
}
