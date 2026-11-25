package control

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Glimesh/waveguide/pkg/types"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Timing configuration for the lifecycle tests. The production default is
// 15s; tests shorten the ticker through Control.heartbeatInterval.
const (
	lcTestInterval = 8 * time.Millisecond
	// lcQuietWindow is comfortably larger than a heartbeat period. If a
	// stopped heartbeat were still alive, multiple ticks would land here.
	lcQuietWindow = 200 * time.Millisecond
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type lcMetadataRecord struct {
	StreamID types.StreamID
	Metadata types.StreamMetadata
}

type lcOrchStart struct {
	ChannelID types.ChannelID
	StreamID  types.StreamID
}

type lcOrchStop struct {
	ChannelID types.ChannelID
	StreamID  types.StreamID
}

type lcFakeService struct {
	mu sync.Mutex

	started  []types.ChannelID
	ended    []types.StreamID
	metadata []lcMetadataRecord
	previews int

	nextStreamID types.StreamID
	endErr       error
}

func (f *lcFakeService) SetLogger(logrus.FieldLogger) {}
func (f *lcFakeService) Name() string                 { return "lc-fake-service" }
func (f *lcFakeService) Connect() error               { return nil }

func (f *lcFakeService) GetHmacKey(types.ChannelID) ([]byte, error) {
	return []byte("key"), nil
}

func (f *lcFakeService) StartStream(channelID types.ChannelID) (types.StreamID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.started = append(f.started, channelID)
	if f.nextStreamID == 0 {
		f.nextStreamID = 500
	}
	id := f.nextStreamID
	f.nextStreamID++
	return id, nil
}

func (f *lcFakeService) EndStream(streamID types.StreamID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ended = append(f.ended, streamID)
	return f.endErr
}

func (f *lcFakeService) UpdateStreamMetadata(streamID types.StreamID, metadata types.StreamMetadata) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.metadata = append(f.metadata, lcMetadataRecord{StreamID: streamID, Metadata: metadata})
	return nil
}

func (f *lcFakeService) SendJpegPreviewImage(types.StreamID, []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.previews++
	return nil
}

func (f *lcFakeService) startedIDs() []types.ChannelID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]types.ChannelID(nil), f.started...)
}

func (f *lcFakeService) endedIDs() []types.StreamID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]types.StreamID(nil), f.ended...)
}

func (f *lcFakeService) metadataRecords() []lcMetadataRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lcMetadataRecord(nil), f.metadata...)
}

func (f *lcFakeService) metadataCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.metadata)
}

type lcFakeOrchestrator struct {
	mu sync.Mutex

	started    []lcOrchStart
	stopped    []lcOrchStop
	heartbeats []types.ChannelID

	startErr     error
	stopErr      error
	heartbeatErr error

	// beforeHeartbeat, when set, is invoked outside the fake lock on every
	// Heartbeat call. Tests use it to pin a tick in flight.
	beforeHeartbeat func()

	// beforeStop, when set, is invoked outside the fake lock at the start of
	// StopStream. Tests use it to pin an external stop call in flight.
	beforeStop func()
}

func (f *lcFakeOrchestrator) SetLogger(logrus.FieldLogger) {}
func (f *lcFakeOrchestrator) Name() string                 { return "lc-fake-orchestrator" }
func (f *lcFakeOrchestrator) Connect() error               { return nil }
func (f *lcFakeOrchestrator) Close() error                 { return nil }

func (f *lcFakeOrchestrator) StartStream(channelID types.ChannelID, streamID types.StreamID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.started = append(f.started, lcOrchStart{ChannelID: channelID, StreamID: streamID})
	return f.startErr
}

func (f *lcFakeOrchestrator) StopStream(channelID types.ChannelID, streamID types.StreamID) error {
	if f.beforeStop != nil {
		f.beforeStop()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.stopped = append(f.stopped, lcOrchStop{ChannelID: channelID, StreamID: streamID})
	return f.stopErr
}

func (f *lcFakeOrchestrator) Heartbeat(channelID types.ChannelID) error {
	if f.beforeHeartbeat != nil {
		f.beforeHeartbeat()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.heartbeats = append(f.heartbeats, channelID)
	return f.heartbeatErr
}

func (f *lcFakeOrchestrator) heartbeatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.heartbeats)
}

func (f *lcFakeOrchestrator) stoppedCalls() []lcOrchStop {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lcOrchStop(nil), f.stopped...)
}

func (f *lcFakeOrchestrator) startedCalls() []lcOrchStart {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lcOrchStart(nil), f.started...)
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type lcHarness struct {
	ctrl *Control
	svc  *lcFakeService
	orch *lcFakeOrchestrator
}

func lcDiscardLogger() logrus.FieldLogger {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	logger.SetLevel(logrus.ErrorLevel)
	return logger
}

func newLCHarness(t *testing.T) *lcHarness {
	t.Helper()

	svc := &lcFakeService{}
	orch := &lcFakeOrchestrator{}

	ctrl := &Control{
		ctx:                context.Background(),
		log:                lcDiscardLogger(),
		service:            svc,
		orchestrator:       orch,
		streams:            make(map[types.ChannelID]*Stream),
		metadataCollectors: make(map[types.ChannelID]chan bool),
		heartbeatInterval:  lcTestInterval,
		// Keep StartStream's ingest goroutine parked on the stream context
		// instead of opening a real WHEP/WebRTC connection.
		ingestRunner: func(ctx context.Context, _ *Stream) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}

	h := &lcHarness{ctrl: ctrl, svc: svc, orch: orch}

	// Guarantee no stream/goroutine leaks if a test fails mid-case.
	t.Cleanup(func() {
		ctrl.Shutdown()
	})

	return h
}

func (h *lcHarness) waitHeartbeats(t *testing.T, atLeast int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return h.orch.heartbeatCount() >= atLeast
	}, 3*time.Second, time.Millisecond*5, "timed out waiting for %d heartbeats", atLeast)
}

func (h *lcHarness) waitMetadata(t *testing.T, atLeast int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return h.svc.metadataCount() >= atLeast
	}, 3*time.Second, time.Millisecond*5, "timed out waiting for %d metadata reports", atLeast)
}

func (h *lcHarness) assertQuiet(t *testing.T, heartbeatCount, metadataCount int) {
	t.Helper()

	time.Sleep(lcQuietWindow)

	assert.Equal(t, heartbeatCount, h.orch.heartbeatCount(),
		"orchestrator received heartbeats after the stream reached its terminal state")
	assert.Equal(t, metadataCount, h.svc.metadataCount(),
		"service received metadata after the stream reached its terminal state")
}

func lcWaitGroupDone(t *testing.T, s *Stream) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		s.heartbeatWG.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat goroutine did not exit after stop")
	}
}

// ---------------------------------------------------------------------------
// Acceptance cases
// ---------------------------------------------------------------------------

// A1: after a normal start/stop, waiting longer than a heartbeat period must
// not produce any more heartbeats or metadata reports.
func TestStreamLifecycle_StartStop_HeartbeatAndMetadataTerminate(t *testing.T) {
	t.Parallel()

	const channelID = types.ChannelID(1001)
	h := newLCHarness(t)

	stream, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err)

	h.waitHeartbeats(t, 3)
	h.waitMetadata(t, 3)

	_, err = h.ctrl.getStream(channelID)
	require.NoError(t, err, "stream should be registered before stop")

	require.NoError(t, h.ctrl.StopStream(channelID))

	assert.True(t, stream.Stopped())
	lcWaitGroupDone(t, stream)

	_, err = h.ctrl.getStream(channelID)
	require.ErrorIs(t, err, errStreamRemoved, "stream table entry must be removed after stop")

	hbAtStop := h.orch.heartbeatCount()
	mdAtStop := h.svc.metadataCount()

	h.assertQuiet(t, hbAtStop, mdAtStop)

	require.Len(t, h.svc.endedIDs(), 1)
	assert.Equal(t, stream.StreamID, h.svc.endedIDs()[0])

	stops := h.orch.stoppedCalls()
	require.Len(t, stops, 1)
	assert.Equal(t, lcOrchStop{ChannelID: channelID, StreamID: stream.StreamID}, stops[0])
}

// A2: repeated sequential stops are nil and trigger external stops once.
func TestStreamLifecycle_RepeatedStop_IsIdempotent(t *testing.T) {
	t.Parallel()

	const channelID = types.ChannelID(1002)
	h := newLCHarness(t)

	stream, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err)
	h.waitHeartbeats(t, 2)

	for i := 0; i < 5; i++ {
		assert.NoError(t, h.ctrl.StopStream(channelID), "stop #%d", i+1)
	}

	// Stopping a channel that never existed / was already removed is a no-op.
	assert.NoError(t, h.ctrl.StopStream(types.ChannelID(9999)))

	assert.Len(t, h.svc.endedIDs(), 1, "EndStream must be called exactly once")
	assert.Len(t, h.orch.stoppedCalls(), 1, "orchestrator StopStream must be called exactly once")

	hbAtStop := h.orch.heartbeatCount()
	mdAtStop := h.svc.metadataCount()
	h.assertQuiet(t, hbAtStop, mdAtStop)

	assert.True(t, stream.Stopped())
}

// A2/A5: concurrent stops while the heartbeat is ticking must collapse to a
// single external stop and must not race. Run with -race.
func TestStreamLifecycle_ConcurrentStops_SingleExternalStop(t *testing.T) {
	t.Parallel()

	const channelID = types.ChannelID(1003)
	h := newLCHarness(t)

	_, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err)
	h.waitHeartbeats(t, 2)

	const callers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- h.ctrl.StopStream(channelID)
		}()
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err)
	}

	assert.Len(t, h.svc.endedIDs(), 1)
	assert.Len(t, h.orch.stoppedCalls(), 1)

	hbAtStop := h.orch.heartbeatCount()
	mdAtStop := h.svc.metadataCount()
	h.assertQuiet(t, hbAtStop, mdAtStop)

	_, err = h.ctrl.getStream(channelID)
	assert.ErrorIs(t, err, errStreamRemoved)
}

// A3: when the business service stop fails, local state still converges and a
// follow-up stop does not error or repeat external calls.
func TestStreamLifecycle_ServiceStopFailure_StillConverges(t *testing.T) {
	t.Parallel()

	const channelID = types.ChannelID(1004)
	h := newLCHarness(t)

	stopErr := errors.New("service unavailable")
	h.svc.endErr = stopErr

	stream, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err)
	h.waitHeartbeats(t, 2)

	err = h.ctrl.StopStream(channelID)
	require.ErrorIs(t, err, stopErr, "the leading caller should observe the external error")

	assert.True(t, stream.Stopped())
	lcWaitGroupDone(t, stream)

	_, err = h.ctrl.getStream(channelID)
	require.ErrorIs(t, err, errStreamRemoved, "table must converge even when external stop fails")

	hbAtStop := h.orch.heartbeatCount()
	mdAtStop := h.svc.metadataCount()
	h.assertQuiet(t, hbAtStop, mdAtStop)

	// Stopping again converges to nil and must not call external stops again.
	assert.NoError(t, h.ctrl.StopStream(channelID))
	assert.NoError(t, h.ctrl.StopStream(channelID))

	assert.Len(t, h.svc.endedIDs(), 1, "failed EndStream must not be re-triggered by later stops")
	assert.Len(t, h.orch.stoppedCalls(), 1, "orchestrator stop still ran exactly once")
}

// A3 variant: orchestrator stop failure behaves the same way.
func TestStreamLifecycle_OrchestratorStopFailure_StillConverges(t *testing.T) {
	t.Parallel()

	const channelID = types.ChannelID(1005)
	h := newLCHarness(t)

	stopErr := errors.New("orchestrator unavailable")
	h.orch.stopErr = stopErr

	stream, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err)
	h.waitHeartbeats(t, 2)

	err = h.ctrl.StopStream(channelID)
	require.ErrorIs(t, err, stopErr)

	lcWaitGroupDone(t, stream)
	_, err = h.ctrl.getStream(channelID)
	require.ErrorIs(t, err, errStreamRemoved)

	hbAtStop := h.orch.heartbeatCount()
	mdAtStop := h.svc.metadataCount()
	h.assertQuiet(t, hbAtStop, mdAtStop)

	assert.NoError(t, h.ctrl.StopStream(channelID))

	assert.Len(t, h.svc.endedIDs(), 1)
	assert.Len(t, h.orch.stoppedCalls(), 1)
}

// A4: a fast stop/start on the same channel establishes a new generation and
// all metadata after the restart belongs to the new StreamID.
func TestStreamLifecycle_QuickRestart_NewGenerationOwnsHeartbeat(t *testing.T) {
	t.Parallel()

	const channelID = types.ChannelID(1006)
	h := newLCHarness(t)

	first, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err)
	h.waitHeartbeats(t, 2)

	require.NoError(t, h.ctrl.StopStream(channelID))
	lcWaitGroupDone(t, first)

	mdAtStop := h.svc.metadataCount()

	// Immediately reconnect: this must not be rejected as "stream already
	// exists" and must not wait on external services.
	second, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err, "quick restart must register a new stream")
	require.NotSame(t, first, second)
	require.NotEqual(t, first.StreamID, second.StreamID)

	assert.True(t, first.Stopped())
	assert.False(t, second.Stopped())

	// The new generation is heartbeating/metadata-reporting.
	h.waitMetadata(t, mdAtStop+3)
	hbBefore := h.orch.heartbeatCount()
	require.Eventually(t, func() bool {
		return h.orch.heartbeatCount() >= hbBefore+3
	}, 3*time.Second, time.Millisecond*5)

	// Every metadata report after the stop boundary must belong to the new
	// generation; a surviving old heartbeat would report the old StreamID.
	records := h.svc.metadataRecords()
	require.GreaterOrEqual(t, len(records), mdAtStop)
	for _, record := range records[mdAtStop:] {
		assert.Equal(t, second.StreamID, record.StreamID,
			"metadata reported by a stale heartbeat generation")
	}

	// The table points at the new generation.
	current, err := h.ctrl.getStream(channelID)
	require.NoError(t, err)
	assert.Same(t, second, current)

	require.NoError(t, h.ctrl.StopStream(channelID))

	ended := h.svc.endedIDs()
	require.Len(t, ended, 2)
	assert.Equal(t, []types.StreamID{first.StreamID, second.StreamID}, ended)

	stops := h.orch.stoppedCalls()
	require.Len(t, stops, 2)
	assert.Equal(t, first.StreamID, stops[0].StreamID)
	assert.Equal(t, second.StreamID, stops[1].StreamID)
}

// A4 stress: many tight stop/start cycles on one channel never collide with a
// stale table entry or heartbeat generation.
func TestStreamLifecycle_RapidStopStartCycles(t *testing.T) {
	t.Parallel()

	const channelID = types.ChannelID(1007)
	h := newLCHarness(t)

	var last *Stream
	for i := 0; i < 25; i++ {
		stream, err := h.ctrl.StartStream(channelID)
		require.NoError(t, err, "start cycle %d", i)
		require.NoError(t, h.ctrl.StopStream(channelID), "stop cycle %d", i)
		require.True(t, stream.Stopped())
		lcWaitGroupDone(t, stream)
		last = stream
	}

	// A final generation still starts and heartbeats normally.
	final, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err)
	h.waitHeartbeats(t, 2)
	require.NoError(t, h.ctrl.StopStream(channelID))
	lcWaitGroupDone(t, final)

	assert.Len(t, h.svc.endedIDs(), 26)
	assert.Len(t, h.orch.stoppedCalls(), 26)
	assert.NotSame(t, last, final)
}

// A5: stopping while a heartbeat tick is in flight must join the heartbeat
// instead of deadlocking or sending into a deleted map channel.
func TestStreamLifecycle_StopWaitsForInFlightHeartbeat(t *testing.T) {
	t.Parallel()

	const channelID = types.ChannelID(1008)
	h := newLCHarness(t)

	hookEntered := make(chan struct{}, 16)
	hookRelease := make(chan struct{})
	var releaseOnce sync.Once
	h.orch.beforeHeartbeat = func() {
		hookEntered <- struct{}{}
		<-hookRelease
	}
	releaseHooks := func() { releaseOnce.Do(func() { close(hookRelease) }) }
	t.Cleanup(releaseHooks)

	stream, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err)

	select {
	case <-hookEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat tick did not start")
	}

	stopResult := make(chan error, 1)
	go func() {
		stopResult <- h.ctrl.StopStream(channelID)
	}()

	// The heartbeat is pinned inside the orchestrator call; a correct stop
	// waits for it rather than returning and leaving it running.
	select {
	case err := <-stopResult:
		t.Fatalf("StopStream returned before the in-flight heartbeat finished: %v", err)
	case <-time.After(120 * time.Millisecond):
	}

	releaseHooks()

	select {
	case err := <-stopResult:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("StopStream deadlocked after releasing the heartbeat")
	}

	lcWaitGroupDone(t, stream)
	assert.Len(t, h.orch.stoppedCalls(), 1)
	assert.Len(t, h.svc.endedIDs(), 1)
}

// A4 overlap: a reconnect that arrives while the old stop is still finishing
// (slow external stop) joins the stopping generation and then registers
// successfully instead of getting "stream already exists".
func TestStreamLifecycle_RestartWhileStopInFlight(t *testing.T) {
	t.Parallel()

	const channelID = types.ChannelID(1011)
	h := newLCHarness(t)

	stopEntered := make(chan struct{}, 16)
	stopRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	t.Cleanup(releaseStop)

	h.orch.beforeStop = func() {
		stopEntered <- struct{}{}
		<-stopRelease
	}

	first, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err)
	h.waitHeartbeats(t, 1)

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- h.ctrl.StopStream(channelID)
	}()

	select {
	case <-stopEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("external stop was not started")
	}

	// The stopping generation has entered Stream.Stop but the table entry is
	// still present while the external call is pinned.
	require.Eventually(t, first.Stopped, time.Second, time.Millisecond*5)

	startDone := make(chan struct {
		stream *Stream
		err    error
	}, 1)
	go func() {
		s, err := h.ctrl.StartStream(channelID)
		startDone <- struct {
			stream *Stream
			err    error
		}{s, err}
	}()

	// Let the reconnect goroutine block joining the old shutdown, then
	// release the external stop.
	time.Sleep(80 * time.Millisecond)
	releaseStop()

	select {
	case err := <-stopDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("first StopStream did not converge after release")
	}

	select {
	case res := <-startDone:
		require.NoError(t, res.err, "reconnect must not be rejected while old generation is stopping")
		require.NotNil(t, res.stream)
		require.NotSame(t, first, res.stream)

		h.waitHeartbeats(t, 2)
		require.NoError(t, h.ctrl.StopStream(channelID))
	case <-time.After(3 * time.Second):
		t.Fatal("reconnect did not finish after old stop converged")
	}
}

// Genuine duplicate publishes for an active (non-stopped) generation must
// still be rejected.
func TestStreamLifecycle_DuplicateActiveStream_Rejected(t *testing.T) {
	t.Parallel()

	const channelID = types.ChannelID(1012)
	h := newLCHarness(t)

	first, err := h.ctrl.StartStream(channelID)
	require.NoError(t, err)

	dup, err := h.ctrl.newStream(channelID, func() {})
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream already exists")
	// Historical newStream behavior returns the constructed value alongside
	// the error; it must not have replaced the registered stream.
	require.NotNil(t, dup)

	current, err := h.ctrl.getStream(channelID)
	require.NoError(t, err)
	assert.Same(t, first, current)
}

// Shutdown stops every registered stream exactly once.
func TestStreamLifecycle_Shutdown_StopsAllStreams(t *testing.T) {
	t.Parallel()

	h := newLCHarness(t)

	const firstChannel = types.ChannelID(1009)
	const secondChannel = types.ChannelID(1010)

	first, err := h.ctrl.StartStream(firstChannel)
	require.NoError(t, err)
	second, err := h.ctrl.StartStream(secondChannel)
	require.NoError(t, err)

	h.waitHeartbeats(t, 2)

	h.ctrl.Shutdown()

	lcWaitGroupDone(t, first)
	lcWaitGroupDone(t, second)

	assert.Len(t, h.svc.endedIDs(), 2)
	assert.Len(t, h.orch.stoppedCalls(), 2)

	// A second shutdown is harmless.
	h.ctrl.Shutdown()
	assert.Len(t, h.svc.endedIDs(), 2)
}
