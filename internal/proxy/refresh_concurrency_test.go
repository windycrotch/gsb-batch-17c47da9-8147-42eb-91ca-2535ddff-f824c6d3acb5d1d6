package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startConcurrent kicks off n goroutines and blocks until every one has
// parked on the returned gate, so tests really exercise concurrent entry
// into the refresh/prefetch paths instead of racing the goroutine scheduler.
func startConcurrent(n int, fn func(i int)) (release func(), done func()) {
	entered := make(chan struct{}, n)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entered <- struct{}{}
			<-gate
			fn(i)
		}(i)
	}
	for range n {
		<-entered
	}
	return func() { close(gate) }, wg.Wait
}

// TestRefreshOnceCoalescesConcurrentRequests proves that a burst of requests
// hitting an expired link triggers the refresh callback exactly once; all
// waiters reuse the single result and the whole session afterwards speaks the
// new URL with the new headers.
func TestRefreshOnceCoalescesConcurrentRequests(t *testing.T) {
	var refreshCalls atomic.Int32
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		switch r.URL.Path {
		case "/old.m3u8":
			if r.Header.Get("Cookie") == "sid=new" {
				t.Error("old path must never be requested with refreshed headers")
			}
			http.Error(w, "expired", http.StatusForbidden)
		case "/new.m3u8":
			if r.Header.Get("Cookie") != "sid=new" {
				http.Error(w, "missing refreshed cookie", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:4,\nseg.ts\n"))
		case "/seg.ts":
			if r.Header.Get("Cookie") != "sid=new" {
				http.Error(w, "segment used stale cookie", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte("SEGMENT"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	refreshEntered := make(chan struct{}, 16)
	refreshGate := make(chan struct{})
	p, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, localURL := p.RegisterSession(SessionConfig{
		URL:      upstream.URL + "/old.m3u8",
		Headers:  map[string]string{"Cookie": "sid=old"},
		Filename: "movie.m3u8",
		Refresh: func(context.Context) (string, map[string]string, error) {
			refreshCalls.Add(1)
			refreshEntered <- struct{}{}
			<-refreshGate
			return upstream.URL + "/new.m3u8", map[string]string{"Cookie": "sid=new"}, nil
		},
	})

	const clients = 20
	type result struct {
		status int
		body   string
	}
	results := make([]result, clients)
	release, done := startConcurrent(clients, func(i int) {
		resp, gerr := http.Get(localURL)
		if gerr != nil {
			results[i] = result{status: -1, body: gerr.Error()}
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		results[i] = result{status: resp.StatusCode, body: string(body)}
	})
	release()
	select {
	case <-refreshEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh callback was never invoked")
	}
	// Give every other request time to park on the in-flight refresh.
	time.Sleep(100 * time.Millisecond)
	close(refreshGate)
	done()

	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh callback ran %d times, want exactly 1", got)
	}
	var segURL string
	for i, res := range results {
		if res.status != http.StatusOK {
			t.Fatalf("request %d got status %d body %q", i, res.status, res.body)
		}
		if !strings.Contains(res.body, "/sub?url=") {
			t.Fatalf("request %d playlist was not rewritten:\n%s", i, res.body)
		}
		for _, line := range strings.Split(res.body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "http://") {
				segURL = strings.TrimSpace(line)
			}
		}
	}
	// HLS sub-resources after the refresh must carry the new headers too.
	segResp, err := http.Get(segURL)
	if err != nil {
		t.Fatal(err)
	}
	segBody, _ := io.ReadAll(segResp.Body)
	segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK || string(segBody) != "SEGMENT" {
		t.Fatalf("sub-resource status=%d body=%q", segResp.StatusCode, segBody)
	}
	if upstreamHits.Load() == 0 {
		t.Fatal("upstream received no requests")
	}
}

// TestRefreshFailureIsHardError covers callback errors, empty addresses and a
// missing refresh callback: the stale link must never be served as success.
func TestRefreshFailureIsHardError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	}))
	defer upstream.Close()

	t.Run("callback error", func(t *testing.T) {
		p, _ := Start(nil)
		defer p.Close()
		_, localURL := p.RegisterSession(SessionConfig{
			URL:     upstream.URL + "/old.mp4",
			Headers: map[string]string{"Cookie": "sid=old"},
			Refresh: func(context.Context) (string, map[string]string, error) {
				return "", nil, errors.New("pan token rejected")
			},
		})
		resp, err := http.Get(localURL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "refresh failed") {
			t.Fatalf("status=%d body=%q, want 502 refresh failure", resp.StatusCode, body)
		}
	})

	t.Run("empty address", func(t *testing.T) {
		p, _ := Start(nil)
		defer p.Close()
		_, localURL := p.RegisterSession(SessionConfig{
			URL:     upstream.URL + "/old.mp4",
			Headers: map[string]string{"Cookie": "sid=old"},
			Refresh: func(context.Context) (string, map[string]string, error) {
				return "   ", map[string]string{"Cookie": "sid=new"}, nil
			},
		})
		resp, err := http.Get(localURL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("empty refresh address produced status=%d, want 502", resp.StatusCode)
		}
		if strings.Contains(string(body), "expired") {
			t.Fatalf("stale upstream body leaked after failed refresh: %q", body)
		}
	})

	t.Run("no callback", func(t *testing.T) {
		p, _ := Start(nil)
		defer p.Close()
		_, localURL := p.RegisterSession(SessionConfig{
			URL:     upstream.URL + "/old.mp4",
			Headers: map[string]string{"Cookie": "sid=old"},
		})
		resp, err := http.Get(localURL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status=%d body=%q, want the explicit upstream 403", resp.StatusCode, body)
		}
	})
}

// TestFailedRefreshDoesNotPoisonLaterAttempts shows a failed refresh is not
// cached as the session's new state: once the upstream can mint a link again,
// the next 401/403 gets a fresh callback invocation and succeeds.
func TestFailedRefreshDoesNotPoisonLaterAttempts(t *testing.T) {
	var allowRefresh atomic.Bool
	var refreshCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old.mp4":
			http.Error(w, "expired", http.StatusForbidden)
		case "/new.mp4":
			if r.Header.Get("Cookie") != "sid=new" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte("FRESH-MEDIA"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	p, _ := Start(nil)
	defer p.Close()
	_, localURL := p.RegisterSession(SessionConfig{
		URL:     upstream.URL + "/old.mp4",
		Headers: map[string]string{"Cookie": "sid=old"},
		Refresh: func(context.Context) (string, map[string]string, error) {
			refreshCalls.Add(1)
			if !allowRefresh.Load() {
				return "", nil, errors.New("refresh temporarily unavailable")
			}
			return upstream.URL + "/new.mp4", map[string]string{"Cookie": "sid=new"}, nil
		},
	})

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("first attempt status=%d, want 502", resp.StatusCode)
	}
	allowRefresh.Store(true)
	resp, err = http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "FRESH-MEDIA" {
		t.Fatalf("retry after recovered refresh: status=%d body=%q", resp.StatusCode, body)
	}
	if got := refreshCalls.Load(); got != 2 {
		t.Fatalf("refresh ran %d times, want 2 independent attempts", got)
	}
}

// TestPrefetchRefreshIsSingleFlightAndBytePerfect drives concurrent Range
// workers against an expired link. The callback must run once, every chunk
// must be re-fetched with the refreshed URL/headers, and the reassembled
// stream must be byte-identical to the source file.
func TestPrefetchRefreshIsSingleFlightAndBytePerfect(t *testing.T) {
	payload := make([]byte, 3*prefetchChunkSize+777) // four chunks
	_, _ = rand.Read(payload)

	var refreshCalls atomic.Int32
	var newCookieRequests atomic.Int32
	refreshEntered := make(chan struct{}, 16)
	refreshGate := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old.mp4" {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		if r.Header.Get("Cookie") != "sid=new" {
			http.Error(w, "chunk missing refreshed cookie", http.StatusForbidden)
			return
		}
		newCookieRequests.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "new.mp4", time.Unix(0, 0), bytes.NewReader(payload))
	}))
	defer upstream.Close()

	p, _ := Start(nil)
	defer p.Close()
	_, localURL := p.RegisterSession(SessionConfig{
		URL:      upstream.URL + "/old.mp4",
		Headers:  map[string]string{"Cookie": "sid=old"},
		Filename: "movie.mp4",
		Prefetch: true,
		Refresh: func(context.Context) (string, map[string]string, error) {
			refreshCalls.Add(1)
			refreshEntered <- struct{}{}
			<-refreshGate
			return upstream.URL + "/new.mp4", map[string]string{"Cookie": "sid=new"}, nil
		},
	})

	const clients = 8
	client := &http.Client{Timeout: 10 * time.Second}
	got := make([][]byte, clients)
	statuses := make([]int, clients)
	release, done := startConcurrent(clients, func(i int) {
		resp, gerr := client.Get(localURL)
		if gerr != nil {
			statuses[i] = -1
			return
		}
		got[i], _ = io.ReadAll(resp.Body)
		statuses[i] = resp.StatusCode
		resp.Body.Close()
	})
	release()
	select {
	case <-refreshEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("prefetch never entered refresh")
	}
	time.Sleep(100 * time.Millisecond) // let the other workers park on the refresh
	close(refreshGate)
	done()

	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("prefetch refresh ran %d times, want 1", got)
	}
	if newCookieRequests.Load() == 0 {
		t.Fatal("no chunk used the refreshed headers")
	}
	for i := range clients {
		if statuses[i] != http.StatusOK {
			t.Fatalf("client %d status=%d", i, statuses[i])
		}
		if !bytes.Equal(got[i], payload) {
			t.Fatalf("client %d got %d bytes, payload is %d bytes: mixed-link splicing", i, len(got[i]), len(payload))
		}
	}
}

// TestPrefetchRejectsShortAndNonPartialChunks makes sure a chunk that cannot
// supply the exact requested byte window aborts instead of silently splicing
// foreign bytes into the stream.
func TestPrefetchRejectsShortAndNonPartialChunks(t *testing.T) {
	t.Run("short partial body", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Advertise a full 1 MiB window but return almost nothing.
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes 0-1048575/5242880")
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("SHORT"))
		}))
		defer upstream.Close()
		p, _ := Start(nil)
		defer p.Close()
		_, localURL := p.RegisterSession(SessionConfig{
			URL: upstream.URL + "/movie.mp4", Filename: "movie.mp4", Prefetch: true,
		})
		resp, err := http.Get(localURL)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("short chunk status=%d, want 502", resp.StatusCode)
		}
	})

	t.Run("upstream ignores range with 200", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Ignore Range entirely: a 200 full-body reply is not a valid
			// ranged chunk, so prefetch must decline and the direct path
			// must serve the body whole instead of splicing it as a chunk.
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("FULL-BODY-IGNORES-RANGE"))
		}))
		defer upstream.Close()
		p, _ := Start(nil)
		defer p.Close()
		_, localURL := p.RegisterSession(SessionConfig{
			URL: upstream.URL + "/movie.mp4", Filename: "movie.mp4", Prefetch: true,
		})
		resp, err := http.Get(localURL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "FULL-BODY-IGNORES-RANGE" {
			t.Fatalf("range-ignoring upstream status=%d body=%q", resp.StatusCode, body)
		}
	})
}

// TestRemoveSessionStopsInFlightPrefetch verifies that removal cancels chunks
// already in flight and prevents any new upstream chunk from starting.
func TestRemoveSessionStopsInFlightPrefetch(t *testing.T) {
	payload := make([]byte, 4*prefetchChunkSize)
	_, _ = rand.Read(payload)

	var active atomic.Int32
	var maxActive atomic.Int32
	var started atomic.Int32 // requests that began real upstream body work
	var afterRemoveWork atomic.Int32
	blockChunks := make(chan struct{})
	var blockOnce sync.Once
	afterRemove := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := active.Add(1)
		for {
			old := maxActive.Load()
			if cur <= old || maxActive.CompareAndSwap(old, cur) {
				break
			}
		}
		defer active.Add(-1)
		if r.Header.Get("Range") != "" && r.Header.Get("Range") != "bytes=0-1048575" {
			blockOnce.Do(func() { close(blockChunks) })
			select {
			case <-blockChunks: // park until the request context is cancelled
			case <-r.Context().Done():
			}
			return
		}
		// Deterministic post-removal barrier: any handler that reaches real
		// body work after RemoveSession is an unambiguous failure, independent
		// of how many queued requests were already in flight.
		select {
		case <-afterRemove:
			afterRemoveWork.Add(1)
			t.Error("upstream started new work after RemoveSession")
			http.Error(w, "removed", http.StatusGone)
			return
		default:
		}
		started.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "movie.mp4", time.Unix(0, 0), bytes.NewReader(payload))
	}))
	defer upstream.Close()

	p, _ := Start(nil)
	defer p.Close()
	id, localURL := p.RegisterSession(SessionConfig{
		URL: upstream.URL + "/movie.mp4", Filename: "movie.mp4", Prefetch: true,
	})

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blockChunks:
	case <-time.After(3 * time.Second):
		t.Fatal("prefetch workers never parked on an in-flight chunk")
	}
	// Read the first chunk so the stream is definitely in flight, then remove.
	buf := make([]byte, 1024)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("first chunk read: %v", err)
	}
	p.RemoveSession(id)
	close(afterRemove)

	// The player side must end (truncated stream) rather than hang forever.
	done := make(chan error, 1)
	go func() {
		_, rerr := io.Copy(io.Discard, resp.Body)
		done <- rerr
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight stream did not end after RemoveSession")
	}
	resp.Body.Close()

	// Drain until every worker has quiesced; the handler itself fails the test
	// if any request crosses the post-removal barrier.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && active.Load() != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if active.Load() != 0 {
		t.Fatalf("%d upstream requests still active after removal", active.Load())
	}
	if started.Load() == 0 {
		t.Fatal("no upstream chunk ever started; the prefetch path did not run")
	}
	if afterRemoveWork.Load() != 0 {
		t.Fatalf("%d upstream requests started after RemoveSession", afterRemoveWork.Load())
	}
	// New playback requests for the removed session fail immediately.
	gone, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	gone.Body.Close()
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("removed session status=%d, want 404", gone.StatusCode)
	}
}

// TestRemoveDuringRefreshNeverRemounts races RemoveSession against an in-flight
// refresh callback. Even when the callback eventually returns a valid address,
// the session must stay removed and every request must fail.
func TestRemoveDuringRefreshNeverRemounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	}))
	defer upstream.Close()

	refreshEntered := make(chan struct{})
	releaseRefresh := make(chan struct{})
	p, _ := Start(nil)
	defer p.Close()
	id, localURL := p.RegisterSession(SessionConfig{
		URL:     upstream.URL + "/old.mp4",
		Headers: map[string]string{"Cookie": "sid=old"},
		Refresh: func(context.Context) (string, map[string]string, error) {
			close(refreshEntered)
			<-releaseRefresh
			return upstream.URL + "/new.mp4", map[string]string{"Cookie": "sid=new"}, nil
		},
	})

	errCh := make(chan error, 1)
	go func() {
		resp, gerr := http.Get(localURL)
		if gerr != nil {
			errCh <- gerr
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		errCh <- nil
	}()
	<-refreshEntered
	p.RemoveSession(id)
	close(releaseRefresh) // callback resolves successfully after removal

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("request hung after remove/refresh race")
	}

	if sess, ok := p.session(id); ok {
		t.Fatalf("session %p still mounted after removal", sess)
	}
	gone, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	gone.Body.Close()
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("removed session remounted with status %d", gone.StatusCode)
	}
}

// TestRefreshOnceUnitBehavior checks the single-flight primitive directly:
// one callback execution, shared result, failed attempt not cached, and empty
// addresses reported as errors.
func TestRefreshOnceUnitBehavior(t *testing.T) {
	p, _ := Start(nil)
	defer p.Close()

	t.Run("success coalesces and atomically swaps state", func(t *testing.T) {
		var calls atomic.Int32
		sess := &proxySession{
			targetURL: "http://example.com/old",
			headers:   map[string]string{"Cookie": "old"},
			removedCh: make(chan struct{}),
			refresh: func(context.Context) (string, map[string]string, error) {
				calls.Add(1)
				time.Sleep(50 * time.Millisecond)
				return "http://example.com/new", map[string]string{"Cookie": "new"}, nil
			},
		}
		const n = 8
		urls := make([]string, n)
		release, done := startConcurrent(n, func(i int) {
			u, h, err := sess.refreshOnce(context.Background())
			if err != nil {
				t.Errorf("refresh %d: %v", i, err)
				return
			}
			urls[i] = u
			if h["Cookie"] != "new" {
				t.Errorf("refresh %d headers=%v", i, h)
			}
		})
		release()
		done()
		if calls.Load() != 1 {
			t.Fatalf("callback ran %d times", calls.Load())
		}
		for i, u := range urls {
			if u != "http://example.com/new" {
				t.Fatalf("waiter %d got %q", i, u)
			}
		}
		if got, h := sess.snapshot(); got != "http://example.com/new" || h["Cookie"] != "new" {
			t.Fatalf("session state not swapped: url=%q headers=%v", got, h)
		}
	})

	t.Run("failure is not cached", func(t *testing.T) {
		var calls atomic.Int32
		sess := &proxySession{
			targetURL: "http://example.com/old",
			headers:   map[string]string{},
			removedCh: make(chan struct{}),
			refresh: func(context.Context) (string, map[string]string, error) {
				if calls.Add(1) == 1 {
					return "", nil, errors.New("boom")
				}
				return "http://example.com/recovered", nil, nil
			},
		}
		if _, _, err := sess.refreshOnce(context.Background()); err == nil {
			t.Fatal("first refresh should fail")
		}
		u, _, err := sess.refreshOnce(context.Background())
		if err != nil || u != "http://example.com/recovered" {
			t.Fatalf("second refresh after failure: url=%q err=%v", u, err)
		}
		if got, _ := sess.snapshot(); got != "http://example.com/recovered" {
			t.Fatalf("stale URL retained: %q", got)
		}
	})

	t.Run("removed session fails even while refreshing", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		sess := &proxySession{
			targetURL: "http://example.com/old",
			headers:   map[string]string{},
			removedCh: make(chan struct{}),
			refresh: func(context.Context) (string, map[string]string, error) {
				close(entered)
				<-release
				return "http://example.com/new", nil, nil
			},
		}
		errCh := make(chan error, 1)
		go func() {
			_, _, err := sess.refreshOnce(context.Background())
			errCh <- err
		}()
		<-entered
		sess.markRemoved()
		close(release)
		err := <-errCh
		if !errors.Is(err, ErrSessionRemoved) {
			t.Fatalf("err=%v, want ErrSessionRemoved", err)
		}
		if sess.alive() {
			t.Fatal("removed session reports alive")
		}
		if got, _ := sess.snapshot(); got != "http://example.com/old" {
			t.Fatalf("removed session state was mutated by the late refresh: %q", got)
		}
	})
}
