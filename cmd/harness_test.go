package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alfg/ffmpegd/ffmpeg"
	"github.com/gorilla/websocket"
)

// ---------- test harness ----------

type harness struct {
	t  *testing.T
	s  *Server
	hs *httptest.Server
}

func newHarness(t *testing.T, queueSize int, r Runner) *harness {
	t.Helper()
	s := NewServer(Options{
		QueueSize:        queueSize,
		Runner:           r,
		AllowAllOrigins:  true,
		TerminalTTL:      time.Minute,
		ProgressInterval: 5 * time.Millisecond,
	})
	s.Start()
	hs := httptest.NewServer(s.Handler())
	h := &harness{t: t, s: s, hs: hs}
	t.Cleanup(func() {
		hs.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return h
}

// newTestServer serves s over a random loopback port.
func newTestServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return hs
}

// testContext returns a shutdown budget tied to the test deadline.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (h *harness) wsURL(owner string) string {
	u := "ws" + strings.TrimPrefix(h.hs.URL, "http") + "/ws"
	if owner != "" {
		u += "?clientId=" + owner
	}
	return u
}

// dial connects and consumes the initial "identified" event.
func (h *harness) dial(owner string) *wsClient {
	h.t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(h.wsURL(owner), nil)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	wc := newWSClient(h.t, c)

	st, _, err := wc.read(2 * time.Second)
	if err != nil {
		h.t.Fatalf("reading identified: %v", err)
	}
	if st.Type != eventIdentified {
		h.t.Fatalf("first event type = %q, want identified", st.Type)
	}
	if owner != "" && st.ClientID != owner {
		h.t.Fatalf("identified clientId = %q, want %q", st.ClientID, owner)
	}
	h.t.Cleanup(wc.close)
	return wc
}

// ---------- websocket client ----------

type wsEvent struct {
	st  Status
	raw map[string]any
}

type wsClient struct {
	conn      *websocket.Conn
	t         *testing.T
	inbox     chan wsEvent
	closed    chan struct{}
	closeOnce sync.Once
}

var (
	errReadTimeout = errors.New("timed out waiting for websocket event")
	errConnClosed  = errors.New("websocket client closed")
)

func newWSClient(t *testing.T, c *websocket.Conn) *wsClient {
	wc := &wsClient{
		conn:   c,
		t:      t,
		inbox:  make(chan wsEvent, 4096),
		closed: make(chan struct{}),
	}
	// A persistent reader avoids setting read deadlines: gorilla retains a
	// deadline error for later reads, which would poison the connection after
	// a negative "no message expected" check.
	go func() {
		defer wc.close()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var st Status
			if err := json.Unmarshal(data, &st); err != nil {
				t.Errorf("unmarshal status %s: %v", string(data), err)
				continue
			}
			var raw map[string]any
			_ = json.Unmarshal(data, &raw)
			select {
			case wc.inbox <- wsEvent{st: st, raw: raw}:
			case <-wc.closed:
				return
			}
		}
	}()
	return wc
}

func (c *wsClient) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (c *wsClient) send(v any) {
	c.t.Helper()
	if err := c.conn.WriteJSON(v); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *wsClient) sendRaw(raw string) {
	c.t.Helper()
	if err := c.conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
		c.t.Fatalf("write raw: %v", err)
	}
}

func (c *wsClient) read(timeout time.Duration) (Status, map[string]any, error) {
	c.t.Helper()
	if timeout <= 0 {
		timeout = time.Millisecond
	}
	select {
	case ev := <-c.inbox:
		return ev.st, ev.raw, nil
	case <-time.After(timeout):
		return Status{}, nil, errReadTimeout
	case <-c.closed:
		return Status{}, nil, errConnClosed
	}
}

// waitFor reads events until pred matches or the deadline elapses.
func (c *wsClient) waitFor(timeout time.Duration, pred func(Status) bool) ([]Status, error) {
	deadline := time.Now().Add(timeout)
	var got []Status
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return got, errReadTimeout
		}
		select {
		case ev := <-c.inbox:
			got = append(got, ev.st)
			if pred(ev.st) {
				return got, nil
			}
		case <-time.After(remaining):
			return got, errReadTimeout
		case <-c.closed:
			return got, errConnClosed
		}
	}
}

// expectSilence fails if any message arrives within within.
func (c *wsClient) expectSilence(within time.Duration, context string) {
	c.t.Helper()
	select {
	case ev := <-c.inbox:
		c.t.Fatalf("%s: expected no message, got type=%q id=%q", context, ev.st.Type, ev.st.TaskID)
	case <-time.After(within):
	case <-c.closed:
	}
}

// drain collects messages for duration and returns them.
func (c *wsClient) drain(duration time.Duration) []Status {
	deadline := time.Now().Add(duration)
	var out []Status
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return out
		}
		select {
		case ev := <-c.inbox:
			out = append(out, ev.st)
		case <-time.After(remaining):
			return out
		case <-c.closed:
			return out
		}
	}
}

// ---------- event predicates / assertions ----------

func isTerminalEvent(st Status) bool {
	switch st.Type {
	case eventCompleted, eventFailed, eventCancelled:
		return true
	}
	return false
}

func countTerminal(events []Status, id string) (completed, failed, cancelled int) {
	for _, st := range events {
		if id != "" && st.TaskID != id {
			continue
		}
		switch st.Type {
		case eventCompleted:
			completed++
		case eventFailed:
			failed++
		case eventCancelled:
			cancelled++
		}
	}
	return
}

func findEvent(events []Status, typ string) *Status {
	for i := range events {
		if events[i].Type == typ {
			return &events[i]
		}
	}
	return nil
}

// ---------- fake runners ----------

type funcRunner func(t *Task, report ProgressFunc) error

func (f funcRunner) Run(t *Task, report ProgressFunc) error { return f(t, report) }

// gatedRunner records launches and blocks each run until released or the task
// context is cancelled.
type gatedRunner struct {
	mu      sync.Mutex
	ran     []string
	started chan *Task
	release chan struct{}
	once    sync.Once
}

func newGatedRunner() *gatedRunner {
	return &gatedRunner{
		started: make(chan *Task, 4096),
		release: make(chan struct{}),
	}
}

func (g *gatedRunner) Run(t *Task, report ProgressFunc) error {
	g.mu.Lock()
	g.ran = append(g.ran, t.Ref)
	g.mu.Unlock()

	g.started <- t
	select {
	case <-t.Context().Done():
		return ffmpeg.ErrCancelled
	case <-g.release:
		return nil
	}
}

func (g *gatedRunner) releaseAll() { g.once.Do(func() { close(g.release) }) }

func (g *gatedRunner) ranRefs() map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := make(map[string]bool, len(g.ran))
	for _, r := range g.ran {
		m[r] = true
	}
	return m
}

func (g *gatedRunner) launchCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.ran)
}

// tickRunner emits progress until cancelled.
type tickRunner struct {
	interval time.Duration
	percent  float64
}

func (tr tickRunner) Run(t *Task, report ProgressFunc) error {
	d := tr.interval
	if d <= 0 {
		d = 5 * time.Millisecond
	}
	pct := tr.percent
	if pct == 0 {
		pct = 12.5
	}
	tk := time.NewTicker(d)
	defer tk.Stop()
	for {
		select {
		case <-t.Context().Done():
			return ffmpeg.ErrCancelled
		case <-tk.C:
			report(Status{Percent: pct, Speed: "1x", FPS: 30})
		}
	}
}

// waitStarted blocks until the gated runner reports a launch.
func waitStarted(t *testing.T, g *gatedRunner, timeout time.Duration) *Task {
	t.Helper()
	select {
	case task := <-g.started:
		return task
	case <-time.After(timeout):
		t.Fatalf("runner did not start within %v", timeout)
		return nil
	}
}
