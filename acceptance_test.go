package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingStreamer wraps the real *YtDlp pipeline and counts how many times
// Stream() would have launched subprocesses. It lets the acceptance tests
// assert the "reject before starting any subprocess" requirement directly,
// while still exercising the real handler entry point and the real process
// pipeline for accepted requests.
type recordingStreamer struct {
	real  *YtDlp
	calls atomic.Int64
}

func (r *recordingStreamer) Stream(ctx context.Context, url string) (*StreamResult, error) {
	r.calls.Add(1)
	return r.real.Stream(ctx, url)
}

// newAcceptanceServer installs mock binaries, builds the real handler with a
// real SessionManager and serves it over a real HTTP listener.
func newAcceptanceServer(t *testing.T, max int, logs *mockLogCollector) (baseURL string, rs *recordingStreamer, sessions *SessionManager) {
	t.Helper()

	ytdlpPath, ffmpegPath := installMockBinaries(t)
	real := &YtDlp{Path: ytdlpPath, FFmpeg: ffmpegPath}
	rs = &recordingStreamer{real: real}
	sessions = NewSessionManager(max)

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", NewStreamHandler(rs, sessions))
	mux.HandleFunc("/health", HealthHandler)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL, rs, sessions
}

func streamURL(base, query string) string {
	target := "https://example.com/watch?v=dQw4w9WgXcQ"
	if query != "" {
		target += "&" + query
	}
	return base + "/stream?url=" + url.QueryEscape(target)
}

// openStream issues a GET and returns the live response; the caller must close
// the body. It fails the test if the request is rejected.
func openStream(t *testing.T, client *http.Client, u string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	return resp
}

// readChunk reads at least one byte from the stream so we know the pipeline is
// running, then returns.
func readChunk(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	return buf[:n]
}

// waitActive waits until the session manager reports the wanted number of
// active sessions.
func waitActive(t *testing.T, sessions *SessionManager, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sessions.Active() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("active sessions = %d, want %d", sessions.Active(), want)
}

func TestAcceptance_ConcurrencyLimitRejectsBeforeSubprocess(t *testing.T) {
	logs := newMockLog(t)
	base, rs, sessions := newAcceptanceServer(t, 2, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	// Fill both slots with live streams.
	var live []*http.Response
	for i := 0; i < 2; i++ {
		resp := openStream(t, client, streamURL(base, "mockmode=long"))
		live = append(live, resp)
		readChunk(t, resp)
	}
	if got := sessions.Active(); got != 2 {
		t.Fatalf("active sessions = %d, want 2", got)
	}
	logs.waitForCount(t, "ffmpeg-start", 2)

	callsBefore := rs.calls.Load()

	// The third request must be rejected with 503 before any subprocess starts.
	resp, err := client.Get(streamURL(base, "mockmode=long"))
	if err != nil {
		t.Fatalf("third request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", resp.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "busy") {
		t.Errorf("body = %q, want it to explain the server is busy", body)
	}
	if got := rs.calls.Load(); got != callsBefore {
		t.Errorf("Stream() calls = %d, want unchanged %d (no subprocess may start for a rejected request)", got, callsBefore)
	}
	if got := logs.count("ytdlp-start"); got != 2 {
		t.Errorf("ytdlp starts = %d, want 2", got)
	}
	if got := logs.count("ffmpeg-start"); got != 2 {
		t.Errorf("ffmpeg starts = %d, want 2", got)
	}

	// Existing sessions are unaffected: they keep receiving audio.
	for i, r := range live {
		if got := readChunk(t, r); len(got) == 0 {
			t.Errorf("session %d stopped producing audio", i)
		}
		r.Body.Close()
	}
}

func TestAcceptance_SlotReturnedAfterNormalCompletion(t *testing.T) {
	logs := newMockLog(t)
	base, _, sessions := newAcceptanceServer(t, 1, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	// A finite stream is served to completion and must free its slot.
	resp := openStream(t, client, streamURL(base, "mockmode=longshot"))
	readChunk(t, resp)
	served, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("reading to completion: %v", err)
	}
	if served == 0 {
		t.Fatalf("served no audio")
	}

	waitActive(t, sessions, 0)
	logs.waitForExit(t, "ytdlp-start", 1)
	logs.waitForExit(t, "ffmpeg-start", 1)

	// A fresh long stream must now be admitted.
	resp2 := openStream(t, client, streamURL(base, "mockmode=long"))
	readChunk(t, resp2)
	resp2.Body.Close()
}

func TestAcceptance_SlotReturnedAfterClientDisconnect(t *testing.T) {
	logs := newMockLog(t)
	base, _, sessions := newAcceptanceServer(t, 1, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	resp := openStream(t, client, streamURL(base, "mockmode=long"))
	readChunk(t, resp)
	logs.waitForCount(t, "ytdlp-start", 1)
	logs.waitForCount(t, "ffmpeg-start", 1)

	if got := sessions.Active(); got != 1 {
		t.Fatalf("active sessions = %d, want 1", got)
	}

	// Simulate the receiver going away mid-stream.
	resp.Body.Close()

	// The slot must come back promptly, and the subprocesses must exit.
	waitActive(t, sessions, 0)
	logs.waitForExit(t, "ytdlp-start", 1)
	logs.waitForExit(t, "ffmpeg-start", 1)

	// A subsequent request must be admitted again on the same server.
	resp2 := openStream(t, client, streamURL(base, "mockmode=long"))
	readChunk(t, resp2)
	resp2.Body.Close()
}

func TestAcceptance_SlotReturnedAfterUpstreamFailure(t *testing.T) {
	logs := newMockLog(t)
	base, _, sessions := newAcceptanceServer(t, 1, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	// yt-dlp exits non-zero and produces no audio: the request fails, but the
	// slot must still be returned.
	resp, err := client.Get(streamURL(base, "mockmode=fail"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	waitActive(t, sessions, 0)

	// Capacity is available again.
	resp2 := openStream(t, client, streamURL(base, "mockmode=long"))
	readChunk(t, resp2)
	resp2.Body.Close()
}

func TestAcceptance_HeadProbeReservesSlotButStartsNoProcess(t *testing.T) {
	logs := newMockLog(t)
	base, rs, sessions := newAcceptanceServer(t, 1, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	req, _ := http.NewRequest(http.MethodHead, streamURL(base, "mockmode=long"), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", ct)
	}

	// HEAD must not launch the pipeline and must release its slot when done.
	if got := rs.calls.Load(); got != 0 {
		t.Errorf("Stream() calls after HEAD = %d, want 0", got)
	}
	if got := logs.count("ytdlp-start"); got != 0 {
		t.Errorf("ytdlp starts after HEAD = %d, want 0", got)
	}
	waitActive(t, sessions, 0)

	// The slot the probe held must be reusable by a real stream.
	resp2 := openStream(t, client, streamURL(base, "mockmode=long"))
	readChunk(t, resp2)
	resp2.Body.Close()
}

func TestAcceptance_HeadProbeOccupiesCapacity(t *testing.T) {
	logs := newMockLog(t)
	base, _, _ := newAcceptanceServer(t, 1, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	// Hold the single slot with a long GET stream.
	resp := openStream(t, client, streamURL(base, "mockmode=long"))
	readChunk(t, resp)
	defer resp.Body.Close()

	// A HEAD probe arriving at capacity must be rejected as busy too.
	req, _ := http.NewRequest(http.MethodHead, streamURL(base, "mockmode=long"), nil)
	head, err := client.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	defer head.Body.Close()
	if head.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("HEAD status = %d, want 503 while at capacity", head.StatusCode)
	}
}

func TestAcceptance_ShutdownTerminatesStreamsAndReturnsSlots(t *testing.T) {
	logs := newMockLog(t)
	base, _, sessions := newAcceptanceServer(t, 2, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	var live []*http.Response
	for i := 0; i < 2; i++ {
		resp := openStream(t, client, streamURL(base, "mockmode=long"))
		live = append(live, resp)
		readChunk(t, resp)
	}
	logs.waitForCount(t, "ytdlp-start", 2)
	logs.waitForCount(t, "ffmpeg-start", 2)

	if got := sessions.Active(); got != 2 {
		t.Fatalf("active = %d, want 2", got)
	}

	// Graceful shutdown must cancel the in-flight streams.
	done := make(chan struct{})
	go func() {
		sessions.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SessionManager.Shutdown blocked")
	}

	// Slots are returned and subprocesses terminate.
	waitActive(t, sessions, 0)
	logs.waitForExit(t, "ytdlp-start", 2)
	logs.waitForExit(t, "ffmpeg-start", 2)

	// The client-visible streams must end (EOF or error) rather than hang.
	for i, r := range live {
		ended := make(chan struct{})
		go func(resp *http.Response) {
			defer close(ended)
			io.Copy(io.Discard, resp.Body)
		}(r)
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Errorf("stream %d did not end after shutdown", i)
		}
		r.Body.Close()
	}

	// After shutdown no new session may start.
	resp, err := client.Get(streamURL(base, "mockmode=long"))
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("post-shutdown status = %d, want 503 (body: %s)", resp.StatusCode, body)
		}
	}
}

func TestAcceptance_DoubleCloseIsSafeAndFreesOneSlot(t *testing.T) {
	logs := newMockLog(t)
	base, _, sessions := newAcceptanceServer(t, 1, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	// Take the only slot and read a little.
	resp := openStream(t, client, streamURL(base, "mockmode=long"))
	readChunk(t, resp)

	body := resp.Body
	body.Close()
	// Closing the same body twice must not panic and must not free extra
	// capacity (which would let two sessions run with max=1).
	body.Close()

	waitActive(t, sessions, 0)

	// Only one session may be admitted at a time now.
	first := openStream(t, client, streamURL(base, "mockmode=long"))
	readChunk(t, first)
	defer first.Body.Close()

	second, err := client.Get(streamURL(base, "mockmode=long"))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second status = %d, want 503 (a double release would have over-freed capacity)", second.StatusCode)
	}
}

// TestAcceptance_ConcurrentBurstOnlyAdmitsCapacity hammers the entry point from
// many goroutines and asserts no more than the configured capacity ever runs
// the pipeline at once, and that every rejected request avoided subprocesses.
func TestAcceptance_ConcurrentBurstOnlyAdmitsCapacity(t *testing.T) {
	logs := newMockLog(t)
	base, rs, sessions := newAcceptanceServer(t, 3, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	const attempts = 12
	var wg sync.WaitGroup
	var admitted, rejected atomic.Int64
	var mu sync.Mutex
	var bodies []io.Closer

	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest(http.MethodGet, streamURL(base, "mockmode=long"), nil)
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			buf := make([]byte, 1)
			if resp.StatusCode == http.StatusOK {
				admitted.Add(1)
				resp.Body.Read(buf)
				mu.Lock()
				bodies = append(bodies, resp.Body)
				mu.Unlock()
			} else {
				rejected.Add(1)
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	close(start)

	// Wait until every request has settled (admitted or rejected).
	deadline := time.Now().Add(10 * time.Second)
	for admitted.Load()+rejected.Load() < attempts && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := admitted.Load() + rejected.Load(); got != attempts {
		t.Fatalf("settled %d/%d requests (admitted=%d rejected=%d)", got, attempts, admitted.Load(), rejected.Load())
	}

	// Long streams never complete on their own, so exactly `capacity` may be
	// admitted while the rest are rejected.
	if got := admitted.Load(); got != 3 {
		t.Fatalf("admitted %d concurrent sessions, want exactly 3 (capacity)", got)
	}
	// Every rejected request must not have started a subprocess: total starts
	// may not exceed the number of admitted sessions.
	logs.waitForCount(t, "ytdlp-start", int(admitted.Load()))
	if got := logs.count("ytdlp-start"); got != int(admitted.Load()) {
		t.Errorf("ytdlp starts = %d, want %d (only admitted sessions may start processes)", got, admitted.Load())
	}
	if got := rs.calls.Load(); got != admitted.Load() {
		t.Errorf("Stream() calls = %d, want %d", got, admitted.Load())
	}

	mu.Lock()
	for _, b := range bodies {
		b.Close()
	}
	mu.Unlock()
	wg.Wait()
	waitActive(t, sessions, 0)
}

// TestAcceptance_StreamDeliversMockAudio verifies the real pipeline actually
// pipes mock yt-dlp output through mock ffmpeg to the HTTP client.
func TestAcceptance_StreamDeliversMockAudio(t *testing.T) {
	logs := newMockLog(t)
	base, _, _ := newAcceptanceServer(t, 1, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	resp := openStream(t, client, streamURL(base, "mockmode=short"))
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	if !scanner.Scan() {
		t.Fatalf("no audio received: %v", scanner.Err())
	}
	if got := scanner.Text(); got != "SHORT-AUDIO-PAYLOAD" {
		t.Errorf("first line = %q, want %q", got, "SHORT-AUDIO-PAYLOAD")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", ct)
	}
}

// TestAcceptance_BusyBodyIsPlainText ensures the 503 response is explanatory.
func TestAcceptance_BusyBodyIsPlainText(t *testing.T) {
	logs := newMockLog(t)
	base, _, _ := newAcceptanceServer(t, 1, logs)
	client := &http.Client{Timeout: 10 * time.Second}

	resp := openStream(t, client, streamURL(base, "mockmode=long"))
	readChunk(t, resp)
	defer resp.Body.Close()

	busy, err := client.Get(streamURL(base, "mockmode=long"))
	if err != nil {
		t.Fatalf("busy request: %v", err)
	}
	defer busy.Body.Close()
	body, _ := io.ReadAll(busy.Body)

	if busy.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", busy.StatusCode)
	}
	if !strings.Contains(string(body), BusyMessage) {
		t.Errorf("body = %q, want to contain %q", body, BusyMessage)
	}
	if ct := busy.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}
