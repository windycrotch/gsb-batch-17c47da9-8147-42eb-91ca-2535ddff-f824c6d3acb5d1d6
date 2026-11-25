package control

import (
	"context"
	"errors"
	"sync"

	"github.com/Glimesh/waveguide/pkg/keyframer"
	"github.com/Glimesh/waveguide/pkg/types"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
)

type StreamTrack struct {
	Type  webrtc.RTPCodecType
	Codec string
	Track webrtc.TrackLocal
}

type Stream struct {
	log logrus.FieldLogger

	cancelFunc context.CancelFunc

	// stopOnce makes local cancellation (Stop) idempotent and closes stopCh
	// exactly once.
	stopOnce sync.Once
	// stopCh is the generation-owned exit signal for all stream goroutines
	// (currently the heartbeat). It is closed by Stop and, unlike the
	// control-level metadataCollectors map entry, is never deleted while
	// goroutines might be selecting on it.
	stopCh chan struct{}

	// shutdownOnce runs the control-plane shutdown (external stop calls and
	// stream-table removal) exactly once for this stream generation.
	shutdownOnce sync.Once
	shutdownErr  error

	// heartbeatWG tracks this generation's heartbeat goroutine. StopStream
	// waits on it so the heartbeat has fully exited before StopStream
	// returns and before a reconnect can register a new generation.
	heartbeatWG sync.WaitGroup

	stateMu sync.RWMutex
	stopped bool

	// authenticated is set after the stream has successfully authed with a remote service
	authenticated bool

	whepURI string

	saveVideo   bool
	videoWriter FileWriter

	// mediaStarted is set after media bytes have come in from the client
	mediaStarted bool
	hasSomeAudio bool
	hasSomeVideo bool

	kf            *keyframer.Keyframer
	rtpIngest     chan *rtp.Packet
	lastThumbnail chan []byte
	// channel used to signal thumbnailer to stop
	stopThumbnailer chan struct{}
	stopHeartbeat   chan struct{}
	// metadataCollector is this generation's metadata/heartbeat exit signal.
	// The same channel is registered in Control.metadataCollectors; keeping a
	// reference on the Stream means deleting the map entry can never strand a
	// heartbeat select on a nil channel.
	metadataCollector chan bool
	requestThumbnail  chan struct{}
	thumbnailReceiver chan *rtp.Packet

	videoWriterChan chan *rtp.Packet

	ChannelID types.ChannelID
	StreamID  types.StreamID
	StreamKey types.StreamKey

	tracks []StreamTrack

	// Raw Metadata
	startTime           int64
	lastTime            int64 // Last time the metadata collector ran
	audioBps            int
	videoBps            int
	totalAudioPackets   int
	totalVideoPackets   int
	lastAudioPackets    int
	lastVideoPackets    int
	clientVendorName    string
	clientVendorVersion string
	videoCodec          string
	audioCodec          string
	videoHeight         int
	videoWidth          int
}

func (s *Stream) AddTrack(track webrtc.TrackLocal, codec string) error {
	// TODO: Needs better support for tracks with different codecs
	if track.Kind() == webrtc.RTPCodecTypeAudio {
		s.hasSomeAudio = true
		s.audioCodec = codec
	} else if track.Kind() == webrtc.RTPCodecTypeVideo {
		s.hasSomeVideo = true
		s.videoCodec = codec
	} else {
		return errors.New("unexpected track kind")
	}

	s.tracks = append(s.tracks, StreamTrack{
		Type:  track.Kind(),
		Track: track,
		Codec: codec,
	})

	return nil
}

func (s *Stream) ReportMetadata(metadatas ...Metadata) error {
	for _, metadata := range metadatas {
		metadata(s)
	}

	return nil
}

// Stop cancels the stream context and signals every stream-owned goroutine
// to exit. It is safe to call repeatedly and concurrently: only the first
// call performs the signals, later calls are no-ops.
func (s *Stream) Stop() {
	if s == nil {
		return
	}

	s.stopOnce.Do(func() {
		if s.log != nil {
			s.log.Infof("stopping stream")
		}

		// Preserve the legacy buffered heartbeat signal for compatibility.
		// The send must never block shutdown if nobody is reading it.
		if s.stopHeartbeat != nil {
			select {
			case s.stopHeartbeat <- struct{}{}:
			default:
			}
		}

		// Broadcast the stop to every goroutine selecting on stopCh.
		if s.stopCh != nil {
			close(s.stopCh)
		}

		if s.cancelFunc != nil {
			s.cancelFunc()
		}

		s.stateMu.Lock()
		s.stopped = true
		s.stateMu.Unlock()

		if s.log != nil {
			s.log.Debug("canceled stream ctx and closed stop signal")
		}
	})
}

func (s *Stream) Stopped() bool {
	if s == nil {
		return true
	}

	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.stopped
}

// logger returns the stream logger or a safe fallback for minimally
// initialized test fixtures.
func (s *Stream) logger() logrus.FieldLogger {
	if s != nil && s.log != nil {
		return s.log
	}
	return logrus.StandardLogger()
}
