package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A full per-connection queue must evict immediately instead of blocking the
// sender (and therefore the dispatcher / other owners) on a slow peer.
func TestHubSendDoesNotBlockOnFullQueue(t *testing.T) {
	h := newHub()
	got := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		got <- c
		<-r.Context().Done()
	}))
	defer hs.Close()

	cc, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	var sc *websocket.Conn
	select {
	case sc = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("server conn not established")
	}

	// Simulate a tracked client whose outbound queue is already full and with
	// no writer draining it.
	full := make(chan any, 1)
	full <- Status{Type: "queued"}
	hc := &hubClient{conn: sc, queue: full, done: make(chan struct{})}
	h.mu.Lock()
	h.conns[sc] = hc
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			h.send(sc, Status{Type: eventProgress})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("send blocked on a full queue")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		_, tracked := h.conns[sc]
		h.mu.Unlock()
		if !tracked {
			return // slow consumer was evicted
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("full-queue client was not evicted")
}
