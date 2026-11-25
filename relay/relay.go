package relay

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrStreamAlreadyExists = errors.New("stream already exists")
	ErrStreamNotExisting   = errors.New("stream does not exist")
)

type RelayConfig struct {
	BufferSize uint
	PacketSize uint
}

type StreamStatistics struct {
	Name    string    `json:"name"`
	URL     string    `json:"url"`
	Clients int       `json:"clients"`
	Created time.Time `json:"created"`
}

// UnpublishFunc releases a stream name. It is idempotent: the first call
// synchronously tears the channel (disconnecting its subscribers) and frees
// the name for a new publisher; subsequent calls are no-ops.
type UnpublishFunc func()

// Relay represents a multi-channel stream relay
type Relay struct {
	mutex    sync.Mutex
	channels map[string]*publisher
	config   *RelayConfig
	pool     *BufferPool
}

// publisher binds a stream name to its channel and the goroutine feeding it.
type publisher struct {
	name    string
	channel *Channel
	sendCh  chan *Buffer

	// closeSendOnce guards closing the publisher-facing send channel,
	// teardownOnce guards the one-shot map/channel teardown body.
	closeSendOnce sync.Once
	teardownOnce  sync.Once
}

// NewRelay creates a relay
func NewRelay(config *RelayConfig) *Relay {
	if config.BufferSize == 0 {
		config.BufferSize = 384000 // 1s @ 3Mbits/s
	}
	if config.PacketSize == 0 {
		config.PacketSize = 1316 // default packet size
	}
	return &Relay{
		channels: make(map[string]*publisher),
		config:   config,
		pool:     NewBufferPool(config.PacketSize),
	}
}

func (s *Relay) AcquireBuffer() *Buffer {
	return s.pool.Get()
}

// Publish claims a stream name for publishing. The returned channel receives
// stream buffers; the returned UnpublishFunc must be called when the publisher
// connection ends (clean close or read error). Teardown is synchronous: once
// it returns the name is free again and all old subscribers have been
// disconnected, so a replacement publisher can claim the name immediately.
func (s *Relay) Publish(name string) (chan<- *Buffer, UnpublishFunc, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, exists := s.channels[name]; exists {
		return nil, nil, ErrStreamAlreadyExists
	}

	pub := &publisher{
		name:    name,
		channel: NewChannel(name, s.config.BufferSize/s.config.PacketSize),
		sendCh:  make(chan *Buffer),
	}
	s.channels[name] = pub

	// Setup publisher goroutine
	go func() {
		for {
			buf, ok := <-pub.sendCh

			// Channel closed: tear the pubsub down from here as a fallback for
			// callers that only close the send channel. Explicit unpublish is
			// the primary path and makes teardown deterministic.
			if !ok {
				s.teardown(pub)
				return
			}

			// Publish buf to subscribers
			pub.channel.Pub(buf)
		}
	}()

	unpublish := func() {
		// Stop accepting further buffers first, then run the synchronous
		// teardown. Each side is guarded by its own once so unpublish and a
		// concurrently-observed channel close can never double-close.
		pub.closeSendOnce.Do(func() {
			close(pub.sendCh)
		})
		s.teardown(pub)
	}
	return pub.sendCh, unpublish, nil
}

// teardown removes the publisher's name from the map and closes its channel.
// It is idempotent and identity-guarded: if another publisher has already
// reclaimed the name it is left untouched, and repeated calls are no-ops.
func (s *Relay) teardown(pub *publisher) {
	pub.teardownOnce.Do(func() {
		s.mutex.Lock()
		// Only delete the map entry while we still own this exact name,
		// otherwise a replacement publisher that claimed it would be torn
		// down by mistake.
		if current, ok := s.channels[pub.name]; ok && current == pub {
			delete(s.channels, pub.name)
		}
		s.mutex.Unlock()

		// Close outside the relay mutex: Channel.Close takes the channel
		// lock and drains queued subscriber buffers, which must not happen
		// while the map lock is held.
		pub.channel.Close()
	})
}

type Subscriber struct {
	ch         chan *Buffer
	chanClosed <-chan struct{}
}

var (
	errUpstreamClosed = errors.New("upstream closed")
	errLateSubscriber = errors.New("late subscriber")
)

func (s *Subscriber) Read() (*Buffer, error) {
	select {
	case <-s.chanClosed:
		return nil, errUpstreamClosed
	case buf, ok := <-s.ch:
		if !ok {
			return nil, errLateSubscriber
		}
		return buf, nil
	}
}

// Subscribe subscribes to a stream by name
func (s *Relay) Subscribe(name string) (*Subscriber, UnsubscribeFunc, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	pub, ok := s.channels[name]
	if !ok {
		return nil, nil, ErrStreamNotExisting
	}
	ch, unsub, err := pub.channel.Sub()
	if err != nil {
		// The channel is mid-teardown even though the map entry lingers;
		// treat it like any other unavailable stream.
		return nil, nil, ErrStreamNotExisting
	}
	return ch, unsub, nil
}

func (s *Relay) GetStatistics() []*StreamStatistics {
	statistics := make([]*StreamStatistics, 0)

	s.mutex.Lock()
	defer s.mutex.Unlock()

	for name, pub := range s.channels {
		stats := pub.channel.Stats()
		statistics = append(statistics, &StreamStatistics{
			Name:    name,
			Clients: stats.clients,
			Created: stats.created,
		})
	}
	return statistics
}

func (s *Relay) ChannelExists(name string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	_, exists := s.channels[name]
	return exists
}
