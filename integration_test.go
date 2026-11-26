// Copyright 2024 Jelly Terra
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0
// that can be found in the LICENSE file and https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

// ---- A minimal RTMP server used to emulate a remote destination ----

type remoteServer struct {
	listener net.Listener
	server   *rtmp.Server

	mu        sync.Mutex
	audioCnt  int64
	videoCnt  int64
	closed    int32
	failAfter int32 // close the underlying conn after this many video messages
	conn      net.Conn
}

type remoteHandler struct {
	rtmp.DefaultHandler
	rs *remoteServer
}

func (h *remoteHandler) OnPublish(_ *rtmp.StreamContext, _ uint32, _ *rtmpmsg.NetStreamPublish) error {
	return nil
}

func (h *remoteHandler) OnAudio(_ uint32, _ io.Reader) error {
	atomic.AddInt64(&h.rs.audioCnt, 1)
	return nil
}

func (h *remoteHandler) OnVideo(_ uint32, payload io.Reader) error {
	_, _ = io.Copy(io.Discard, payload)
	n := atomic.AddInt64(&h.rs.videoCnt, 1)
	if f := atomic.LoadInt32(&h.rs.failAfter); f > 0 && n >= int64(f) {
		// Simulate a remote that dies mid-stream, breaking subsequent writes.
		h.rs.mu.Lock()
		c := h.rs.conn
		h.rs.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	}
	return nil
}

func (h *remoteHandler) OnClose() {
	atomic.StoreInt32(&h.rs.closed, 1)
}

func startRemoteServer(t *testing.T, failAfter int32) *remoteServer {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen remote: %v", err)
	}

	rs := &remoteServer{listener: l, failAfter: failAfter}
	rs.server = rtmp.NewServer(&rtmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *rtmp.ConnConfig) {
			rs.mu.Lock()
			rs.conn = conn
			rs.mu.Unlock()
			return conn, &rtmp.ConnConfig{
				Handler: &remoteHandler{rs: rs},
				ControlState: rtmp.StreamControlStateConfig{
					DefaultBandwidthWindowSize: 6 * 1024 * 1024 / 8,
				},
			}
		},
	})

	go func() { _ = rs.server.Serve(l) }()
	t.Cleanup(func() { _ = rs.server.Close() })

	return rs
}

func (rs *remoteServer) addr() string {
	host, port, _ := net.SplitHostPort(rs.listener.Addr().String())
	return host + ":" + port
}

func (rs *remoteServer) videos() int64 { return atomic.LoadInt64(&rs.videoCnt) }
func (rs *remoteServer) audios() int64 { return atomic.LoadInt64(&rs.audioCnt) }

// ---- A minimal RTMP publisher client ----

type publishClient struct {
	client *rtmp.ClientConn
	stream *rtmp.Stream
}

// newPublisher connects to pioneer using a standards-compliant TCURL, so the
// pioneer handler can parse the route from the URL path.
func newPublisher(t *testing.T, pioneerAddr, routeApp, key string) *publishClient {
	t.Helper()

	client, err := rtmp.Dial("rtmp", pioneerAddr, &rtmp.ConnConfig{})
	if err != nil {
		t.Fatalf("publisher dial: %v", err)
	}

	if err := client.Connect(&rtmpmsg.NetConnectionConnect{
		Command: rtmpmsg.NetConnectionConnectCommand{
			App:   routeApp,
			TCURL: "rtmp://" + pioneerAddr + "/" + routeApp,
		},
	}); err != nil {
		_ = client.Close()
		t.Fatalf("publisher connect: %v", err)
	}

	stream, err := client.CreateStream(&rtmpmsg.NetConnectionCreateStream{}, 128)
	if err != nil {
		_ = client.Close()
		t.Fatalf("publisher create stream: %v", err)
	}

	if err := stream.Publish(&rtmpmsg.NetStreamPublish{
		PublishingName: key,
		PublishingType: "live",
	}); err != nil {
		_ = client.Close()
		t.Fatalf("publisher publish: %v", err)
	}

	return &publishClient{client: client, stream: stream}
}

func (p *publishClient) sendVideo(ts uint32) error {
	return p.stream.Write(6, ts, &rtmpmsg.VideoMessage{
		Payload: bytes.NewReader(testVideoPayload),
	})
}

func (p *publishClient) sendAudio(ts uint32) error {
	return p.stream.Write(5, ts, &rtmpmsg.AudioMessage{
		Payload: bytes.NewReader(testAudioPayload),
	})
}

func (p *publishClient) close() {
	_ = p.client.Close()
}

// ---- Helpers to run the pioneer server with a profile route ----

func startPioneer(t *testing.T, recDir, profDir string) (string, *rtmp.Server) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pioneer listen: %v", err)
	}

	srv := rtmp.NewServer(&rtmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *rtmp.ConnConfig) {
			return conn, &rtmp.ConnConfig{
				Handler: &Handler{
					HandleFunc: handleConn,
					OutDir:     recDir,
					ProfileDir: profDir,
				},
				ControlState: rtmp.StreamControlStateConfig{
					DefaultBandwidthWindowSize: 6 * 1024 * 1024 / 8,
				},
			}
		},
	})

	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	return l.Addr().String(), srv
}

func writeProfile(t *testing.T, dir, name string, profile Profile) {
	t.Helper()
	b, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// End to end: with two healthy remotes and recording enabled, all outputs keep
// receiving media after one remote dies mid-stream; the recording stays playable.
func TestIntegrationSingleRemoteFailureKeepsOthersStreaming(t *testing.T) {
	recDir := t.TempDir()
	profileDir := t.TempDir()

	goodRemote := startRemoteServer(t, 0)
	flakyRemote := startRemoteServer(t, 3) // dies after 3 videos

	writeProfile(t, profileDir, "multi", Profile{
		Recording: true,
		Remotes: []StreamServerConfig{
			{Host: flakyRemote.addr(), Path: "live", Key: "k"},
			{Host: goodRemote.addr(), Path: "live", Key: "k"},
		},
	})

	addr, _ := startPioneer(t, recDir, profileDir)
	pub := newPublisher(t, addr, "profile/multi", "ignored")
	defer pub.close()

	// Stream a burst of media. The flaky remote breaks early; pioneer must
	// isolate it and keep delivering to the good remote and the recording.
	for i := 0; i < 30; i++ {
		if err := pub.sendVideo(uint32(i * 40)); err != nil {
			break // publisher itself may notice its own conn only on close
		}
		_ = pub.sendAudio(uint32(i * 40))
		time.Sleep(2 * time.Millisecond)
	}

	// The healthy remote must have received media beyond the flaky remote's
	// failure point.
	waitFor(t, "good remote receives continued media", 3*time.Second, func() bool {
		return goodRemote.videos() >= 20
	})

	if got := flakyRemote.videos(); got > 5 {
		t.Fatalf("flaky remote should have stopped early, got %d videos", got)
	}
	if got := goodRemote.videos(); got < 20 {
		t.Fatalf("good remote should keep receiving, only got %d videos", got)
	}

	pub.close()

	// The recording must be a complete, parseable FLV.
	entries, _ := os.ReadDir(recDir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly one recording, got %d", len(entries))
	}
	tags, _ := parseFlvStrict(t, filepath.Join(recDir, entries[0].Name()))
	if tags == 0 {
		t.Fatal("recording contains no tags")
	}
}

// End to end: when every remote refuses the connection and recording is off,
// the publish must fail explicitly instead of pretending the session started.
func TestIntegrationAllRemotesFailAndNoRecordingFailsSession(t *testing.T) {
	recDir := t.TempDir()
	profileDir := t.TempDir()

	// A closed port guarantees a connection failure.
	deadPort := pickClosedPort(t)

	writeProfile(t, profileDir, "allfail", Profile{
		Recording: false,
		Remotes: []StreamServerConfig{
			{Host: deadPort, Path: "live", Key: "k"},
		},
	})

	addr, _ := startPioneer(t, recDir, profileDir)

	client, err := rtmp.Dial("rtmp", addr, &rtmp.ConnConfig{})
	if err != nil {
		t.Fatalf("dial pioneer: %v", err)
	}
	defer client.Close()

	if err := client.Connect(&rtmpmsg.NetConnectionConnect{
		Command: rtmpmsg.NetConnectionConnectCommand{
			App:   "profile/allfail",
			TCURL: "rtmp://" + addr + "/profile/allfail",
		},
	}); err != nil {
		t.Fatalf("connect pioneer: %v", err)
	}

	stream, err := client.CreateStream(&rtmpmsg.NetConnectionCreateStream{}, 128)
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}

	// Publish itself is a fire-and-forget RTMP command: the server's setup
	// failure surfaces as the inbound connection being torn down, which makes a
	// subsequent write fail (the client observes "connection closed").
	_ = stream.Publish(&rtmpmsg.NetStreamPublish{
		PublishingName: "ignored",
		PublishingType: "live",
	})

	deadline := time.Now().Add(3 * time.Second)
	var writeErr error
	for time.Now().Before(deadline) {
		if writeErr = stream.Write(6, 0, &rtmpmsg.VideoMessage{
			Payload: bytes.NewReader(testVideoPayload),
		}); writeErr != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if writeErr == nil {
		t.Fatal("session must fail (connection closed) when all remotes are down and recording is disabled")
	}
}

// End to end: one dead remote is skipped while the healthy one connects and
// receives media, so the session still starts successfully.
func TestIntegrationOneDeadRemoteIsSkipped(t *testing.T) {
	recDir := t.TempDir()
	profileDir := t.TempDir()

	goodRemote := startRemoteServer(t, 0)
	deadPort := pickClosedPort(t)

	writeProfile(t, profileDir, "partial", Profile{
		Recording: false,
		Remotes: []StreamServerConfig{
			{Host: deadPort, Path: "live", Key: "k"},
			{Host: goodRemote.addr(), Path: "live", Key: "k"},
		},
	})

	addr, _ := startPioneer(t, recDir, profileDir)
	pub := newPublisher(t, addr, "profile/partial", "ignored")
	defer pub.close()

	for i := 0; i < 10; i++ {
		_ = pub.sendVideo(uint32(i * 40))
		time.Sleep(2 * time.Millisecond)
	}

	waitFor(t, "healthy remote receives media despite dead peer", 3*time.Second, func() bool {
		return goodRemote.videos() >= 8
	})
}

func pickClosedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// ---- Webhook test harness ----

type webhookCounters struct {
	beforeConnect int64
	afterStart    int64
	afterClose    int64
}

// startWebhookServer exposes success endpoints. Failure webhooks point at a
// closed port (see failingHook), because DoWebhook treats HTTP status codes as
// success and only returns an error on transport failure - matching the
// production semantics.
func startWebhookServer(t *testing.T, counters *webhookCounters) (base string, failing *WebhookConfig) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/before", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&counters.beforeConnect, 1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/start", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&counters.afterStart, 1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/close", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&counters.afterClose, 1)
		w.WriteHeader(http.StatusOK)
	})

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("webhook listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	base = "http://" + l.Addr().String()
	// A port that is guaranteed closed -> http.DefaultClient.Do returns error.
	failing = hook("http://"+pickClosedPort(t), "/fail")
	return base, failing
}

func hook(base, path string) *WebhookConfig {
	return &WebhookConfig{
		Method:       http.MethodPost,
		URL:          base + path,
		BodyEncoding: "string",
		Body:         "",
	}
}

// afterStart failure keeps the original "abort the session" semantics, but the
// already-established remote connection must be closed (no dangling conn) and
// the afterClose webhook must still run exactly once.
func TestIntegrationAfterStartFailureClosesOutputsAndRunsAfterClose(t *testing.T) {
	recDir := t.TempDir()
	profileDir := t.TempDir()

	goodRemote := startRemoteServer(t, 0)
	counters := &webhookCounters{}
	hookBase, failingHook := startWebhookServer(t, counters)

	writeProfile(t, profileDir, "hookfail", Profile{
		Recording: true,
		Remotes: []StreamServerConfig{
			{Host: goodRemote.addr(), Path: "live", Key: "k"},
		},
		Webhooks: map[string][]*WebhookConfig{
			"afterStart": {failingHook},
			"afterClose": {hook(hookBase, "/close")},
		},
	})

	addr, _ := startPioneer(t, recDir, profileDir)
	client, err := rtmp.Dial("rtmp", addr, &rtmp.ConnConfig{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := client.Connect(&rtmpmsg.NetConnectionConnect{
		Command: rtmpmsg.NetConnectionConnectCommand{
			App:   "profile/hookfail",
			TCURL: "rtmp://" + addr + "/profile/hookfail",
		},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	stream, err := client.CreateStream(&rtmpmsg.NetConnectionCreateStream{}, 128)
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}
	_ = stream.Publish(&rtmpmsg.NetStreamPublish{PublishingName: "k", PublishingType: "live"})

	// The session aborts, so the inbound connection is closed.
	waitFor(t, "inbound connection torn down after afterStart failure", 3*time.Second, func() bool {
		return stream.Write(6, 0, &rtmpmsg.VideoMessage{
			Payload: bytes.NewReader(testVideoPayload),
		}) != nil
	})
	_ = client.Close()

	waitFor(t, "afterClose webhook fired once", 3*time.Second, func() bool {
		return atomic.LoadInt64(&counters.afterClose) == 1
	})

	// The remote that connected before the hook failure must have been closed.
	waitFor(t, "established remote closed during abort", 3*time.Second, func() bool {
		return atomic.LoadInt32(&goodRemote.closed) == 1
	})

	// The recording created before the hook failure must be a finalized, valid
	// FLV (9-byte header at minimum), never a half-written file. An empty-but-
	// valid FLV is acceptable here since no media had been written yet.
	entries, _ := os.ReadDir(recDir)
	if len(entries) != 1 {
		t.Fatalf("expected one recording file, got %d", len(entries))
	}
	if _, size := parseFlvStrict(t, filepath.Join(recDir, entries[0].Name())); size < 9 {
		t.Fatalf("recording was not finalized properly, size=%d", size)
	}
}

// beforeConnect failure aborts before any remote is connected; afterClose must
// not be registered for a session that never started, and nothing dangles.
func TestIntegrationBeforeConnectFailureAbortsCleanly(t *testing.T) {
	recDir := t.TempDir()
	profileDir := t.TempDir()

	goodRemote := startRemoteServer(t, 0)
	counters := &webhookCounters{}
	hookBase, failingHook := startWebhookServer(t, counters)

	writeProfile(t, profileDir, "beforefail", Profile{
		Recording: true,
		Remotes: []StreamServerConfig{
			{Host: goodRemote.addr(), Path: "live", Key: "k"},
		},
		Webhooks: map[string][]*WebhookConfig{
			"beforeConnect": {failingHook},
			"afterClose":    {hook(hookBase, "/close")},
		},
	})

	addr, _ := startPioneer(t, recDir, profileDir)
	client, err := rtmp.Dial("rtmp", addr, &rtmp.ConnConfig{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := client.Connect(&rtmpmsg.NetConnectionConnect{
		Command: rtmpmsg.NetConnectionConnectCommand{
			App:   "profile/beforefail",
			TCURL: "rtmp://" + addr + "/profile/beforefail",
		},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	stream, err := client.CreateStream(&rtmpmsg.NetConnectionCreateStream{}, 128)
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}
	_ = stream.Publish(&rtmpmsg.NetStreamPublish{PublishingName: "k", PublishingType: "live"})

	waitFor(t, "inbound connection torn down after beforeConnect failure", 3*time.Second, func() bool {
		return stream.Write(6, 0, &rtmpmsg.VideoMessage{
			Payload: bytes.NewReader(testVideoPayload),
		}) != nil
	})
	_ = client.Close()

	// Give any erroneously-issued webhook a moment to land.
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt64(&counters.afterClose); got != 0 {
		t.Fatalf("afterClose must not fire for a session aborted in beforeConnect, got %d", got)
	}
	if got := atomic.LoadInt32(&goodRemote.closed); got != 0 {
		t.Fatalf("no remote should have been connected before beforeConnect failed")
	}
}

// When every remote fails to connect but recording is enabled, the session
// still starts and the recording keeps receiving media.
func TestIntegrationAllRemotesFailButRecordingSucceeds(t *testing.T) {
	recDir := t.TempDir()
	profileDir := t.TempDir()

	deadPort := pickClosedPort(t)

	writeProfile(t, profileDir, "recsonly", Profile{
		Recording: true,
		Remotes: []StreamServerConfig{
			{Host: deadPort, Path: "live", Key: "k"},
		},
	})

	addr, _ := startPioneer(t, recDir, profileDir)
	pub := newPublisher(t, addr, "profile/recsonly", "ignored")

	for i := 0; i < 10; i++ {
		if err := pub.sendVideo(uint32(i * 40)); err != nil {
			t.Fatalf("session should stay alive via recording, write failed: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	pub.close()

	entries, _ := os.ReadDir(recDir)
	if len(entries) != 1 {
		t.Fatalf("expected one recording, got %d", len(entries))
	}
	tags, _ := parseFlvStrict(t, filepath.Join(recDir, entries[0].Name()))
	if tags < 10 {
		t.Fatalf("recording should capture all media, got %d tags", tags)
	}
}
