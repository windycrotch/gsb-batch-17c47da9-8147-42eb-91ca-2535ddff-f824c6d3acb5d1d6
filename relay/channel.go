package relay

import (
	"errors"
	"log"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/voc/srtrelay/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const relaySubsystem = "relay"

var (
	activeClients = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: prometheus.BuildFQName(metrics.Namespace, relaySubsystem, "active_clients"),
			Help: "The number of active clients per channel",
		},
		[]string{"channel_name"},
	)
	channelCreatedTimestamp = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: prometheus.BuildFQName(metrics.Namespace, relaySubsystem, "created_timestamp_seconds"),
			Help: "The UNIX timestamp when the channel was created",
		},
		[]string{"channel_name"},
	)
)

// ErrChannelClosed is returned when interacting with a channel that has been torn down.
var ErrChannelClosed = errors.New("channel closed")

type UnsubscribeFunc func()

type Channel struct {
	name       string
	mutex      sync.Mutex
	subs       Subs
	maxPackets uint
	closed     chan struct{}
	closeOnce  sync.Once

	// statistics
	clients atomic.Value
	created time.Time

	// Prometheus metrics.
	activeClients    prometheus.Gauge
	createdTimestamp prometheus.Gauge
}
type Subs []*Subscriber

type Stats struct {
	clients int
	created time.Time
}

// Remove removes a single subscriber, draining queued buffers back to the
// pool before closing its delivery channel. It reports whether the subscriber
// was present, so callers can keep their accounting idempotent.
func (subs Subs) Remove(sub *Subscriber) (Subs, bool) {
	idx := -1
	for i := range subs {
		if subs[i] == sub {
			idx = i
			break
		}
	}

	// subscriber was already removed
	if idx < 0 {
		return subs, false
	}

	for {
		select {
		case buf := <-sub.ch:
			if buf != nil {
				buf.Release()
			}
		default:
			close(sub.ch)
			return slices.Delete(subs, idx, idx+1), true
		}
	}
}

func NewChannel(name string, maxPackets uint) *Channel {
	channelActiveClients := activeClients.WithLabelValues(name)
	ch := &Channel{
		name:          name,
		subs:          make(Subs, 0, 10),
		maxPackets:    maxPackets,
		created:       time.Now(),
		activeClients: channelActiveClients,
		closed:        make(chan struct{}),
	}
	ch.clients.Store(0)
	ch.createdTimestamp = channelCreatedTimestamp.WithLabelValues(name)
	ch.createdTimestamp.Set(float64(ch.created.UnixNano()) / 1000000.0)
	return ch
}

// Sub subscribes to a channel. It fails once the channel has been closed so
// that a torn-down channel can never accumulate new subscribers or metrics.
func (ch *Channel) Sub() (*Subscriber, UnsubscribeFunc, error) {
	ch.mutex.Lock()
	defer ch.mutex.Unlock()

	if ch.isClosed() {
		return nil, nil, ErrChannelClosed
	}

	sub := &Subscriber{
		ch:         make(chan *Buffer, ch.maxPackets),
		chanClosed: ch.closed,
	}
	ch.subs = append(ch.subs, sub)
	ch.clients.Store(len(ch.subs))
	ch.activeClients.Inc()

	var unsub UnsubscribeFunc = func() {
		ch.mutex.Lock()
		defer ch.mutex.Unlock()

		// Channel already closed: its teardown owns all remaining accounting.
		if ch.subs == nil {
			return
		}

		// Idempotent: an overflow drop (or a previous unsubscribe) already
		// removed this subscriber and accounted for it.
		var removed bool
		ch.subs, removed = ch.subs.Remove(sub)
		if !removed {
			return
		}
		ch.clients.Store(len(ch.subs))
		ch.activeClients.Dec()
	}
	return sub, unsub, nil
}

func (ch *Channel) isClosed() bool {
	select {
	case <-ch.closed:
		return true
	default:
		return false
	}
}

// Pub publishes a packet to a channel
func (ch *Channel) Pub(b *Buffer) {
	ch.mutex.Lock()
	defer ch.mutex.Unlock()

	// A closed channel no longer delivers anything; the publisher relay has
	// been torn down and queued buffers are reclaimed by Close.
	if ch.isClosed() {
		return
	}

	toRemove := make(Subs, 0, 5)
	for i := range ch.subs {
		b.Retain()
		select {
		case ch.subs[i].ch <- b:
			continue

		// Remember overflowed chans for drop
		default:
			b.Release()
			toRemove = append(toRemove, ch.subs[i])
			log.Println("dropping overflowing client", i)
		}
	}
	for _, sub := range toRemove {
		var removed bool
		ch.subs, removed = ch.subs.Remove(sub)
		if removed {
			ch.activeClients.Dec()
		}
	}
	ch.clients.Store(len(ch.subs))
	b.Release()
}

// Close closes a channel. It is idempotent and safe to call concurrently:
// subscribers are removed explicitly so queued buffers return to the pool and
// all statistics settle at zero.
func (ch *Channel) Close() {
	ch.closeOnce.Do(func() {
		ch.mutex.Lock()
		defer ch.mutex.Unlock()

		close(ch.closed)
		// Remove subscribers explicitly to return queued buffers to the pool.
		for len(ch.subs) > 0 {
			ch.subs, _ = ch.subs.Remove(ch.subs[0])
		}
		ch.subs = nil
		ch.clients.Store(0)
		activeClients.DeleteLabelValues(ch.name)
		channelCreatedTimestamp.DeleteLabelValues(ch.name)
	})
}

func (ch *Channel) Stats() Stats {
	return Stats{
		clients: ch.clients.Load().(int),
		created: ch.created,
	}
}
