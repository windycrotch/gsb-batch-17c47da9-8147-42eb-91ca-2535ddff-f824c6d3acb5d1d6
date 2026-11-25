package control

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"sync"
	"time"

	"github.com/Glimesh/waveguide/config"
	"github.com/Glimesh/waveguide/pkg/h264"
	"github.com/Glimesh/waveguide/pkg/keyframer"
	"github.com/Glimesh/waveguide/pkg/orchestrator"
	"github.com/Glimesh/waveguide/pkg/service"
	"github.com/Glimesh/waveguide/pkg/types"
	"github.com/pion/rtp"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type Pipe struct {
	Input        string
	Output       string
	Orchestrator string
}

type Control struct {
	ctx context.Context

	// mu guards streams and metadataCollectors. It must never be held while
	// waiting on a stream heartbeat or calling external services.
	mu sync.Mutex

	service      service.Service
	orchestrator orchestrator.Orchestrator
	streams      map[types.ChannelID]*Stream

	metadataCollectors map[types.ChannelID]chan bool

	log     logrus.FieldLogger
	httpMux *http.ServeMux

	// heartbeatInterval, when > 0, overrides the production 15s heartbeat
	// ticker. It is intended for tests that need to exercise multiple
	// heartbeat periods quickly.
	heartbeatInterval time.Duration

	// ingestRunner, when set, replaces the default (*Stream).Ingest media
	// loop launched by StartStream. It is intended for lifecycle tests that
	// must not open a WHEP/WebRTC connection.
	ingestRunner func(ctx context.Context, stream *Stream) error

	Hostname       string
	HTTPServerType string `mapstructure:"http_server_type"`
	HTTPAddress    string `mapstructure:"http_address"`
	HTTPS          bool
	HTTPSHostname  string `mapstructure:"https_hostname"`
	HTTPSCert      string `mapstructure:"https_cert"`
	HTTPSKey       string `mapstructure:"https_key"`

	// Flag to enable saving video stream to file
	// Currently it's global flag toggled from the config file
	SaveVideo bool `mapstructure:"save_video"`
}

func New(
	ctx context.Context,
	cfg config.Config,
	hostname string,
	logger *logrus.Logger,
) (*Control, error) {
	svc := service.New(cfg, logger)
	if err := svc.Connect(); err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}

	or := orchestrator.New(cfg, hostname, logger)
	if err := or.Connect(); err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}

	httpCfg := cfg.Control

	return &Control{
		ctx:          ctx,
		service:      svc,
		orchestrator: or,

		streams:            make(map[types.ChannelID]*Stream),
		metadataCollectors: make(map[types.ChannelID]chan bool),
		httpMux:            http.NewServeMux(),
		log: logger.WithFields(logrus.Fields{
			"control": "waveguide",
		}),

		Hostname:       hostname,
		HTTPServerType: httpCfg.HTTPServerType,
		HTTPAddress:    httpCfg.Address,
		HTTPSHostname:  httpCfg.HTTPSHostname,
		HTTPSCert:      httpCfg.HTTPSCert,
		HTTPSKey:       httpCfg.HTTPSKey,

		// this should be controlled at a stream level
		SaveVideo: cfg.Control.SaveVideo,
	}, nil
}

func (ctrl *Control) Context() context.Context {
	if ctrl.ctx == nil {
		// A zero-value Control (for example a lightweight unit-test fixture)
		// still gets a usable root context.
		return context.Background()
	}
	return ctrl.ctx
}

func (ctrl *Control) ContextErr() error {
	return ctrl.Context().Err()
}

func (ctrl *Control) logger() logrus.FieldLogger {
	if ctrl.log != nil {
		return ctrl.log
	}
	return logrus.StandardLogger()
}

func (ctrl *Control) Shutdown() {
	ctrl.mu.Lock()
	channelIDs := make([]types.ChannelID, 0, len(ctrl.streams))
	for channelID := range ctrl.streams {
		channelIDs = append(channelIDs, channelID)
	}
	ctrl.mu.Unlock()

	for _, channelID := range channelIDs {
		ctrl.StopStream(channelID)
	}
}

func (ctrl *Control) GetTracks(channelID types.ChannelID) ([]StreamTrack, error) {
	stream, err := ctrl.getStream(channelID)
	if err != nil {
		return nil, err
	}

	return stream.tracks, nil
}

func (ctrl *Control) GetHmacKey(channelID types.ChannelID) (string, error) {
	actualKey, err := ctrl.service.GetHmacKey(channelID)
	if err != nil {
		return "", err
	}

	return string(actualKey), nil
}

func (ctrl *Control) Authenticate(channelID types.ChannelID, streamKey types.StreamKey) error {
	actualKey, err := ctrl.service.GetHmacKey(channelID)
	if err != nil {
		return err
	}
	if string(streamKey) != string(actualKey) {
		return errors.New("incorrect stream key")
	}

	return nil
}

func (ctrl *Control) StartStream(channelID types.ChannelID) (*Stream, error) {
	ctx, cancel := context.WithCancel(ctrl.Context())

	stream, err := ctrl.newStream(channelID, cancel)
	if err != nil {
		// A previous generation for this channel may already be shutting
		// down (its StopStream is still finishing, for example a slow
		// external stop call). Rejecting the reconnect with "stream already
		// exists" here is the reported quick-reconnect bug. If the existing
		// generation has already been asked to stop, join its terminal
		// shutdown and retry registration exactly once.
		if stopping := ctrl.stoppingStream(channelID); stopping != nil {
			_ = ctrl.stopStreamInstance(stopping)
			stream, err = ctrl.newStream(channelID, cancel)
		}

		if err != nil {
			cancel()
			return nil, err
		}
	}

	ctrl.logger().Infof("Starting stream for %s", channelID)

	streamID, err := ctrl.service.StartStream(channelID)
	if err != nil {
		ctrl.removeStream(channelID)
		cancel()
		return nil, err
	}
	stream.StreamID = streamID

	err = ctrl.orchestrator.StartStream(stream.ChannelID, stream.StreamID)
	if err != nil {
		// Heartbeat has not been launched yet, so the heartbeat WaitGroup
		// token count is zero and StopStream can converge immediately.
		ctrl.StopStream(channelID)
		return nil, err
	}

	// Account for the heartbeat goroutine before launching it. StopStream
	// waits on this WaitGroup, which guarantees the heartbeat of this
	// generation has fully exited before StopStream returns.
	stream.heartbeatWG.Add(1)
	go func() {
		defer stream.heartbeatWG.Done()
		ctrl.setupHeartbeat(channelID)
	}()

	go func() {
		runIngest := ctrl.ingestRunner
		if runIngest == nil {
			runIngest = func(ctx context.Context, s *Stream) error {
				return s.Ingest(ctx)
			}
		}

		if err := runIngest(ctx, stream); err != nil { //nolint not shadowed
			stream.logger().Error(err)
			// Stop the exact generation this ingest belongs to, never
			// whatever currently owns the channel ID: a fast reconnect may
			// already have registered a brand new stream.
			ctrl.stopStreamInstance(stream)
		}
	}()

	return stream, err
}

// StopStream stops the stream currently registered for channelID.
//
// It is idempotent: stopping an unknown / already removed channel returns
// nil. For a registered stream the shutdown sequence (signal goroutines,
// wait for the heartbeat to exit, call external stop endpoints once, remove
// the table entry) runs exactly once per stream generation. Only the caller
// that leads that single shutdown observes the external-service error;
// concurrent and follow-up callers block until shutdown has converged and
// then receive nil without triggering any additional external stop calls.
func (ctrl *Control) StopStream(channelID types.ChannelID) error {
	ctrl.logger().Debug("Stop Stream")

	ctrl.mu.Lock()
	ctrl.ensureMapsLocked()
	stream := ctrl.streams[channelID]
	ctrl.mu.Unlock()

	if stream == nil {
		// Already stopped and removed (or never started): nothing to do.
		return nil
	}

	return ctrl.stopStreamInstance(stream)
}

// stopStreamInstance is the generation-scoped counterpart of StopStream.
// External stop calls happen inside stream.shutdownOnce and therefore at
// most once for the lifetime of this stream.
func (ctrl *Control) stopStreamInstance(stream *Stream) error {
	if stream == nil {
		return nil
	}

	leader := false
	stream.shutdownOnce.Do(func() {
		leader = true
		stream.shutdownErr = ctrl.shutdownStream(stream)
	})

	if leader {
		return stream.shutdownErr
	}
	return nil
}

// shutdownStream performs the actual terminal cleanup for one stream
// generation. It always drives local state to terminal (stop signals closed,
// heartbeat joined, table entry removed) even when an external stop call
// fails; the first external error is returned to the leading caller.
func (ctrl *Control) shutdownStream(stream *Stream) error {
	channelID := stream.ChannelID

	// 1. Cancel the stream context and close the generation stop signal.
	stream.Stop()

	// 2. Preserve the historical metadata-collector notification. The send
	// is buffered/non-blocking; correctness does not depend on it because
	// the heartbeat also selects on the generation-owned stopCh.
	ctrl.notifyMetadataCollector(stream)
	ctrl.logger().Debug("sent metadata collector signal")

	// 3. Wait until this generation's heartbeat goroutine has fully exited.
	// After this returns there can be no further metadata or heartbeat call
	// from the old stream.
	stream.heartbeatWG.Wait()

	// 4. Make sure stop commands are sent to everyone, exactly once.
	var serviceErr error
	if ctrl.service != nil {
		serviceErr = ctrl.service.EndStream(stream.StreamID)
		if serviceErr != nil {
			stream.logger().Error(serviceErr)
		}
	}

	var orchestratorErr error
	if ctrl.orchestrator != nil {
		orchestratorErr = ctrl.orchestrator.StopStream(stream.ChannelID, stream.StreamID)
		if orchestratorErr != nil {
			stream.logger().Error(orchestratorErr)
		}
	}

	// 5. Always remove the stream table entry so the channel can reconnect.
	// Only delete when the table still points at this generation: a fast
	// reconnect may already have installed a new stream for the channel.
	ctrl.mu.Lock()
	if current, ok := ctrl.streams[channelID]; ok && current == stream {
		delete(ctrl.streams, channelID)
	}
	if current, ok := ctrl.metadataCollectors[channelID]; ok &&
		stream.metadataCollector != nil && current == stream.metadataCollector {
		delete(ctrl.metadataCollectors, channelID)
	}
	ctrl.mu.Unlock()

	if serviceErr != nil {
		return serviceErr
	}
	if orchestratorErr != nil {
		return orchestratorErr
	}

	return nil
}

// notifyMetadataCollector delivers the legacy bool signal to the collector
// channel owned by the stream generation. It never blocks shutdown.
func (ctrl *Control) notifyMetadataCollector(stream *Stream) {
	collector := stream.metadataCollector
	if collector == nil {
		ctrl.mu.Lock()
		collector = ctrl.metadataCollectors[stream.ChannelID]
		ctrl.mu.Unlock()
	}
	if collector == nil {
		return
	}

	select {
	case collector <- true:
	default:
		// A signal is already pending, or the heartbeat has exited; stopCh
		// guarantees termination either way.
	}
}

// defaultHeartbeatInterval is the production heartbeat cadence.
const defaultHeartbeatInterval = 15 * time.Second

// heartbeatInterval is an optional package-level test override for the
// heartbeat cadence. Tests may assign it directly; zero keeps the default.
// Control.heartbeatInterval takes precedence when positive.
var heartbeatInterval time.Duration

func (ctrl *Control) heartbeatPeriod() time.Duration {
	if ctrl.heartbeatInterval > 0 {
		return ctrl.heartbeatInterval
	}
	if heartbeatInterval > 0 {
		return heartbeatInterval
	}
	return defaultHeartbeatInterval
}

var (
	ErrHeartbeatThumbnail             = errors.New("error sending thumbnail")
	ErrHeartbeatSendMetadata          = errors.New("error sending metadata")
	ErrHeartbeatOrchestratorHeartbeat = errors.New("error sending orchestrator heartbeat")
)

func (ctrl *Control) setupHeartbeat(channelID types.ChannelID) {
	ticker := time.NewTicker(ctrl.heartbeatPeriod())
	defer ticker.Stop()
	tickFailed := 0

	stream, err := ctrl.getStream(channelID)
	if err != nil {
		return
	}

	// Resolve the collector channel once, for this generation. The select
	// must never re-index ctrl.metadataCollectors: removeStream deletes that
	// map entry and a missing key behaves like a nil channel, which would
	// leave the heartbeat running and reporting against a reconnecting
	// generation.
	ctrl.mu.Lock()
	collector := ctrl.metadataCollectors[channelID]
	ctrl.mu.Unlock()
	if collector == nil {
		collector = stream.metadataCollector
	}

	for {
		select {
		case <-stream.stopCh:
			return
		case <-collector:
			return
		case <-ticker.C:
			stream.logger().Infof("Collecting metadata tickFailed=%d", tickFailed)
			var err error
			hasErrors := false

			err = ctrl.sendThumbnailFor(stream)
			if err != nil {
				stream.logger().Error(errors.Wrap(err, ErrHeartbeatThumbnail.Error()))
				hasErrors = true
			}

			err = ctrl.sendMetadataFor(stream)
			if err != nil {
				stream.logger().Error(errors.Wrap(err, ErrHeartbeatSendMetadata.Error()))
				hasErrors = true
			}

			err = ctrl.orchestrator.Heartbeat(stream.ChannelID)
			if err != nil {
				stream.logger().Error(errors.Wrap(err, ErrHeartbeatOrchestratorHeartbeat.Error()))
				hasErrors = true
			}

			if hasErrors {
				tickFailed++
			} else if tickFailed > 0 {
				tickFailed--
			}

			// Look for consecutive failures
			if tickFailed >= 5 {
				stream.logger().Warn("Stopping stream due to excessive heartbeat errors")
				stream.Stop()
				// Run the full control-plane cleanup from another goroutine:
				// shutdown waits for this heartbeat goroutine (and its
				// WaitGroup token) to finish exiting.
				go ctrl.stopStreamInstance(stream)
				return
			}
		}
	}
}

func (ctrl *Control) sendMetadata(channelID types.ChannelID) error {
	stream, err := ctrl.getStream(channelID)
	if err != nil {
		return err
	}

	return ctrl.sendMetadataFor(stream)
}

func (ctrl *Control) sendMetadataFor(stream *Stream) error {
	stream.lastTime = time.Now().Unix()

	return ctrl.service.UpdateStreamMetadata(stream.StreamID, types.StreamMetadata{
		AudioCodec:        stream.audioCodec,
		IngestServer:      ctrl.Hostname,
		IngestViewers:     0,
		LostPackets:       0, // Don't exist
		NackPackets:       0, // Don't exist
		RecvPackets:       stream.totalAudioPackets + stream.totalVideoPackets,
		SourceBitrate:     0, // Likely just need to calculate the bytes between two 5s snapshots?
		SourcePing:        0, // Not accessible unless we ping them manually
		StreamTimeSeconds: int(stream.lastTime - stream.startTime),
		VendorName:        stream.clientVendorName,
		VendorVersion:     stream.clientVendorVersion,
		VideoCodec:        stream.videoCodec,
		VideoHeight:       stream.videoHeight,
		VideoWidth:        stream.videoWidth,
	})
}

func (ctrl *Control) sendThumbnail(channelID types.ChannelID) error {
	stream, err := ctrl.getStream(channelID)
	if err != nil {
		return err
	}

	return ctrl.sendThumbnailFor(stream)
}

func (ctrl *Control) sendThumbnailFor(stream *Stream) (err error) {
	// signal the stream thumnailer to get me some thumbnails
	select {
	case stream.requestThumbnail <- struct{}{}:
	default:
	}

	ctrl.logger().Debug("thumbnail requested")

	var data []byte
	// Since stream.lastThumbnail is a buffered chan, let's read all values to get the newest
	for len(stream.lastThumbnail) > 0 {
		data = <-stream.lastThumbnail
	}

	if len(data) == 0 {
		return nil
	}

	var img image.Image
	h264dec, err := h264.NewH264Decoder()
	if err != nil {
		return err
	}
	defer h264dec.Close()
	img, err = h264dec.Decode(data)
	if err != nil {
		return err
	}
	if img == nil {
		ctrl.logger().WithField("channel_id", stream.ChannelID).Debug("img is nil")
		return nil
	}

	buff := new(bytes.Buffer)
	err = jpeg.Encode(buff, img, &jpeg.Options{
		Quality: 75,
	})
	if err != nil {
		return err
	}

	err = ctrl.service.SendJpegPreviewImage(stream.StreamID, buff.Bytes())
	if err != nil {
		return err
	}

	ctrl.logger().WithField("channel_id", stream.ChannelID).Debug("Got screenshot!")

	// Also update our metadata
	stream.videoWidth = img.Bounds().Dx()
	stream.videoHeight = img.Bounds().Dy()

	return nil
}

func (ctrl *Control) newStream(channelID types.ChannelID, cancelFunc context.CancelFunc) (*Stream, error) {
	log := ctrl.logger()

	metadataCollector := make(chan bool, 1)

	stream := &Stream{ //nolint exhaustive struct
		ChannelID: channelID,

		log:           log.WithField("channel_id", channelID),
		whepURI:       ctrl.HTTPServerURL() + "/whep/endpoint/" + channelID.String(),
		authenticated: true,

		cancelFunc:        cancelFunc,
		stopCh:            make(chan struct{}),
		metadataCollector: metadataCollector,
		kf:                keyframer.New(),
		rtpIngest:         make(chan *rtp.Packet),
		stopHeartbeat:     make(chan struct{}, 1),
		stopThumbnailer:   make(chan struct{}, 1),
		thumbnailReceiver: make(chan *rtp.Packet, 50),
		requestThumbnail:  make(chan struct{}, 1),

		lastThumbnail: make(chan []byte, 1),

		startTime: time.Now().Unix(),
	}

	// TODO: this shouldn't be a global flag
	// but rather configured on per stream basis
	if ctrl.SaveVideo {
		stream.saveVideo = true
		stream.videoWriterChan = make(chan *rtp.Packet, 100) // not sure what the buffer size here should be
	}

	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	ctrl.ensureMapsLocked()

	if _, exists := ctrl.streams[channelID]; exists {
		return stream, errors.New("stream already exists in stream manager state")
	}
	ctrl.streams[channelID] = stream
	ctrl.metadataCollectors[channelID] = metadataCollector

	return stream, nil
}

// ensureMapsLocked lazily initializes the state maps so zero-value Control
// fixtures remain usable. Callers must hold ctrl.mu.
func (ctrl *Control) ensureMapsLocked() {
	if ctrl.streams == nil {
		ctrl.streams = make(map[types.ChannelID]*Stream)
	}
	if ctrl.metadataCollectors == nil {
		ctrl.metadataCollectors = make(map[types.ChannelID]chan bool)
	}
}

func (ctrl *Control) removeStream(id types.ChannelID) error {
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()

	ctrl.ensureMapsLocked()
	if _, exists := ctrl.streams[id]; !exists {
		return errors.New("RemoveStream stream does not exist in state")
	}

	delete(ctrl.streams, id)
	delete(ctrl.metadataCollectors, id)

	return nil
}

var errStreamRemoved = errors.New("stream does not exist in state")

// stoppingStream returns the registered generation for id only when it has
// already entered its terminal stop state. An active stream returns nil so
// genuine duplicate publishes keep being rejected with "stream already
// exists".
func (ctrl *Control) stoppingStream(id types.ChannelID) *Stream {
	ctrl.mu.Lock()
	ctrl.ensureMapsLocked()
	stream := ctrl.streams[id]
	ctrl.mu.Unlock()

	if stream != nil && stream.Stopped() {
		return stream
	}
	return nil
}

func (ctrl *Control) getStream(id types.ChannelID) (*Stream, error) {
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()

	ctrl.ensureMapsLocked()
	stream, exists := ctrl.streams[id]
	if !exists {
		return nil, errStreamRemoved
	}
	return stream, nil
}
