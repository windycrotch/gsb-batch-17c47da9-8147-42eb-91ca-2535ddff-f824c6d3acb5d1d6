/*
Copyright 2022-2025 The nagare media authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmafingest_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"github.com/nagare-media/ingest/internal/cmaftest"
	"github.com/nagare-media/ingest/pkg/app"
	"github.com/nagare-media/ingest/pkg/app/cmafingest"
	"github.com/nagare-media/ingest/pkg/config/v1alpha1"
	"github.com/nagare-media/ingest/pkg/event"
	"github.com/nagare-media/ingest/pkg/function"
	"github.com/nagare-media/ingest/pkg/function/manifest"
	"github.com/nagare-media/ingest/pkg/http/router"
	"github.com/nagare-media/ingest/pkg/volume"
	"github.com/nagare-media/ingest/pkg/volume/mem"
)

const (
	testPathPrefix = "/test"
	testStreamName = "example"

	testVideoSet   = "video"
	testVideoTrack = "video.cmvf"
	testAudioSet   = "audio"
	testAudioTrack = "audio.cmfa"

	// testTimescale and testSampleDuration define fragment durations of exactly one second:
	// 30 samples * 3000 ticks / 90000 ticks-per-second = 1.0s.
	testTimescale      = 90000
	testSampleDuration = 3000
	testSampleCount    = 30
)

// env is a fully wired ingest + manifest pipeline driven through a real HTTP server.
type env struct {
	t   *testing.T
	ln  net.Listener
	srv *fiber.App

	vol    volume.Volume
	cmaf   app.App
	fn     function.Function
	client *http.Client

	baseURL string
	host    string
	prefix  string

	cancel context.CancelFunc
}

func newEnv(t *testing.T, streamTimeout time.Duration) *env {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	log := zap.NewNop().Sugar()
	if os.Getenv("INGEST_TEST_LOG") != "" {
		l, _ := zap.NewDevelopment()
		log = l.Sugar()
	}

	vol, err := mem.New(v1alpha1.Volume{
		Name:   "data",
		Memory: &v1alpha1.MemoryVolume{GarbageCollectionPeriod: time.Hour},
	})
	if err != nil {
		cancel()
		t.Fatalf("failed to create mem volume: %v", err)
	}
	if err := vol.Init(volumeExecCtx{log}); err != nil {
		cancel()
		t.Fatalf("failed to init volume: %v", err)
	}

	cmafApp, err := cmafingest.New(v1alpha1.App{
		Name: "cmaf",
		CMAFIngest: &v1alpha1.CMAFIngest{
			VolumeRef:          v1alpha1.Reference{Name: "data"},
			StreamTimeout:      streamTimeout,
			MaxHeaderSize:      1 << 20,
			MaxChunkHeaderSize: 1 << 20,
			MaxChunkMdatSize:   8 << 20,
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("failed to create cmaf ingest app: %v", err)
	}

	testApp := &testApp{cfg: cmafApp.Config(), stream: cmafApp.EventStream(), ready: make(chan struct{})}
	execCtx := &testExecCtx{
		log:    log,
		prefix: testPathPrefix,
		reg:    staticRegistry{vol},
		app:    testApp,
	}

	cmafApp.SetExecCtx(execCtx)
	cmafApp.SetCtx(ctx)

	fn, err := manifest.New(v1alpha1.Function{
		Name:     "manifest",
		Manifest: &v1alpha1.ManifestFunction{VolumeRef: v1alpha1.Reference{Name: "data"}},
	})
	if err != nil {
		cancel()
		t.Fatalf("failed to create manifest function: %v", err)
	}

	// Start the manifest function before any event is published and wait until it has subscribed.
	fnDone := make(chan error, 1)
	go func() { fnDone <- fn.Exec(ctx, execCtx) }()
	select {
	case <-testApp.ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("manifest function did not subscribe to the event stream")
	}

	// HTTP server with the ingest routes.
	fiberApp := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		StreamRequestBody:     true,
	})
	r := router.New(fiberApp)
	registrar, ok := cmafApp.(interface {
		RegisterHTTPRoutes(router.Router, bool) error
	})
	if !ok {
		cancel()
		t.Fatal("cmaf ingest app does not register HTTP routes")
	}
	if err := registrar.RegisterHTTPRoutes(r, false); err != nil {
		cancel()
		t.Fatalf("failed to register HTTP routes: %v", err)
	}

	// A read-only HTTP entry that serves the ingested files from the same volume, mirroring what an HLS player would
	// fetch from a generic-serve deployment. This lets tests assert on playlists and segments over HTTP, not just by
	// reading the backing volume.
	r.Get("/+", func(c *fiber.Ctx) error {
		// The route pattern "/+" captures the full path after the leading slash, including the "/test" app prefix.
		filePath := path.Join("/", c.Params("+"))
		f, err := vol.Open(filePath)
		if err != nil {
			return fiber.ErrNotFound
		}
		fr, err := f.AcquireReader()
		if err != nil {
			return fiber.ErrNotFound
		}
		body, err := io.ReadAll(fr)
		_ = fr.Close()
		if err != nil {
			return fiber.ErrInternalServerError
		}
		if path.Ext(filePath) == ".m3u8" {
			c.Response().Header.SetContentType("application/vnd.apple.mpegurl")
		}
		return c.Send(body)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatalf("failed to listen: %v", err)
	}
	go func() { _ = fiberApp.Listener(ln) }()

	// Use a fixed Host header so the stream name (and therefore the volume path) is deterministic.
	const testHostHeader = "ingest.test"

	e := &env{
		t:       t,
		ln:      ln,
		srv:     fiberApp,
		vol:     vol,
		cmaf:    cmafApp,
		fn:      fn,
		client:  &http.Client{Timeout: 60 * time.Second},
		baseURL: "http://" + ln.Addr().String(),
		host:    testHostHeader,
		prefix:  testPathPrefix,
		cancel:  cancel,
	}

	t.Cleanup(func() {
		_ = fiberApp.Shutdown()
		cancel()
		<-fnDone
		_ = vol.Finalize(volumeExecCtx{log})
	})

	return e
}

// hostDir is the stream-name component derived from the request's Host header. The ingest app namespaces streams by
// host.
func (e *env) hostDir() string { return e.host }

// streamBase is the volume path under which the stream's files are stored.
func (e *env) streamBase() string {
	return path.Join(e.prefix, e.hostDir(), testStreamName+".str")
}

func (e *env) masterPath() string { return path.Join(e.streamBase(), "master.m3u8") }

func (e *env) mediaPath(set, track string) string {
	return path.Join(e.streamBase(), set, track, "index.m3u8")
}

func (e *env) segmentPath(set, track, name string) string {
	return path.Join(e.streamBase(), set, track, name)
}

func (e *env) uploadURL(set, track string) string {
	return fmt.Sprintf("%s/%s.str/Switching(%s)/Streams(%s)", e.baseURL, testStreamName, set, track)
}

// readFile reads a file from the backing volume.
func (e *env) readFile(p string) (string, bool) {
	f, err := e.vol.Open(p)
	if err != nil {
		return "", false
	}
	r, err := f.AcquireReader()
	if err != nil {
		return "", false
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// getHTTP fetches a file over the read-only HTTP entry, as an HLS player would. The returned status is -1 on a
// transport error.
func (e *env) getHTTP(p string) (int, string) {
	req, err := http.NewRequest(http.MethodGet, e.baseURL+p, nil)
	if err != nil {
		return -1, ""
	}
	req.Host = e.host
	resp, err := e.client.Do(req)
	if err != nil {
		return -1, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waitFor polls cond until it returns true or the deadline expires.
func (e *env) waitFor(desc string, timeout time.Duration, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", desc)
}

// upload is a long-running chunked upload that can be driven incrementally.
type upload struct {
	t    *testing.T
	pw   *io.PipeWriter
	done chan uploadResult
}

type uploadResult struct {
	status int
	err    error
}

func (e *env) startUpload(set, track string) *upload {
	e.t.Helper()

	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, e.uploadURL(set, track), pr)
	if err != nil {
		e.t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("DASH-IF-Ingest", "1.2")
	req.Host = e.host
	req.ContentLength = -1 // chunked transfer encoding

	u := &upload{t: e.t, pw: pw, done: make(chan uploadResult, 1)}
	go func() {
		resp, err := e.client.Do(req)
		if err != nil {
			u.done <- uploadResult{err: err}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		u.done <- uploadResult{status: resp.StatusCode}
	}()

	return u
}

func (u *upload) write(b []byte) {
	u.t.Helper()
	if _, err := u.pw.Write(b); err != nil {
		u.t.Fatalf("failed to write upload body: %v", err)
	}
}

func (u *upload) close() {
	_ = u.pw.Close()
}

func (u *upload) abort() {
	_ = u.pw.CloseWithError(fmt.Errorf("client aborted upload"))
}

func (u *upload) wait(timeout time.Duration) uploadResult {
	u.t.Helper()
	select {
	case res := <-u.done:
		return res
	case <-time.After(timeout):
		u.t.Fatalf("timed out waiting for upload response")
		return uploadResult{}
	}
}

func videoTrack() cmaftest.Track {
	return cmaftest.Track{
		HandlerType: "vide",
		Timescale:   testTimescale,
		Width:       1280,
		Height:      720,
	}
}

func audioTrack() cmaftest.Track {
	return cmaftest.Track{
		HandlerType: "soun",
		Timescale:   testTimescale,
	}
}

// fragment builds a one-second, sync-sample fragment at the given fragment index.
func fragment(index int) []byte {
	return cmaftest.FragmentOptions{
		SequenceNumber:      uint32(index + 1),
		BaseMediaDecodeTime: uint64(index) * testSampleCount * testSampleDuration,
		SampleCount:         testSampleCount,
		SampleDuration:      testSampleDuration,
		SampleSize:          32,
		Sync:                true,
	}.Fragment()
}

func fragmentName(index int) string {
	return fmt.Sprintf("frag-%d.m4s", uint64(index)*testSampleCount*testSampleDuration)
}

// uploadTrack uploads a complete track (init + fragments) and waits for the response.
func (e *env) uploadTrack(set, track string, trk cmaftest.Track, fragments int) uploadResult {
	e.t.Helper()
	u := e.startUpload(set, track)
	u.write(trk.InitSegment())
	for i := 0; i < fragments; i++ {
		u.write(fragment(i))
	}
	u.close()
	return u.wait(30 * time.Second)
}

// --- playlist parsing -------------------------------------------------------

type mediaPlaylist struct {
	raw        string
	targetDur  string
	initURI    string
	segments   []string
	endlist    bool
	versionHdr bool
	hasMap     bool
}

// tryParseMediaPlaylist parses a media playlist without failing the test. It returns an error for malformed content.
func tryParseMediaPlaylist(raw string) (mediaPlaylist, error) {
	pl := mediaPlaylist{raw: raw, hasMap: false}

	if !strings.HasPrefix(raw, "#EXTM3U\n") {
		return pl, fmt.Errorf("media playlist does not start with #EXTM3U")
	}
	if !strings.HasSuffix(raw, "\n") {
		return pl, fmt.Errorf("media playlist is truncated (no trailing newline)")
	}

	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")

	pendingExtinf := false
	seenTarget := false
	for _, line := range lines {
		switch {
		case line == "":
			return pl, fmt.Errorf("unexpected empty line")
		case strings.HasPrefix(line, "#EXT-X-VERSION:"):
			pl.versionHdr = true
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			pl.targetDur = strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")
			seenTarget = true
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			pl.initURI = line
			pl.hasMap = true
		case line == "#EXT-X-ENDLIST":
			pl.endlist = true
		case strings.HasPrefix(line, "#EXTINF:"):
			if pendingExtinf {
				return pl, fmt.Errorf("two #EXTINF lines without a segment URI in between")
			}
			pendingExtinf = true
		case strings.HasPrefix(line, "#"):
			// other tags are ignored
		default:
			if !pendingExtinf {
				return pl, fmt.Errorf("segment URI without preceding #EXTINF: %q", line)
			}
			pendingExtinf = false
			pl.segments = append(pl.segments, line)
		}
	}
	if pendingExtinf {
		return pl, fmt.Errorf("playlist ends with an #EXTINF without a segment URI (truncated)")
	}
	if !pl.versionHdr || !seenTarget {
		return pl, fmt.Errorf("playlist misses required tags")
	}

	return pl, nil
}

func parseMediaPlaylist(t *testing.T, raw string) mediaPlaylist {
	t.Helper()
	pl, err := tryParseMediaPlaylist(raw)
	if err != nil {
		t.Fatalf("%v:\n%s", err, raw)
	}
	return pl
}

type masterPlaylist struct {
	raw     string
	audio   []string
	video   []string
	streams []string
}

func parseMasterPlaylist(t *testing.T, raw string) masterPlaylist {
	t.Helper()
	mp := masterPlaylist{raw: raw}

	if !strings.HasPrefix(raw, "#EXTM3U\n") {
		t.Fatalf("master playlist does not start with #EXTM3U:\n%s", raw)
	}
	if !strings.HasSuffix(raw, "\n") {
		t.Fatalf("master playlist is truncated (no trailing newline):\n%s", raw)
	}

	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	pendingStream := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			mp.audio = append(mp.audio, line)
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			if pendingStream {
				t.Fatalf("two #EXT-X-STREAM-INF lines without a URI in between:\n%s", raw)
			}
			pendingStream = true
			mp.video = append(mp.video, line)
		case line == "" || strings.HasPrefix(line, "#"):
			// ignore
		default:
			if !pendingStream {
				t.Fatalf("unexpected non-tag line in master playlist: %q", line)
			}
			pendingStream = false
			mp.streams = append(mp.streams, line)
		}
	}
	if pendingStream {
		t.Fatalf("master playlist ends with a #EXT-X-STREAM-INF without a URI (truncated):\n%s", raw)
	}

	return mp
}

// --- fakes -----------------------------------------------------------------

type testApp struct {
	cfg    v1alpha1.App
	stream event.Stream
	once   sync.Once
	ready  chan struct{}
}

func (a *testApp) Config() v1alpha1.App           { return a.cfg }
func (a *testApp) SetCtx(ctx context.Context)     {}
func (a *testApp) SetExecCtx(execCtx app.ExecCtx) {}
func (a *testApp) EventStream() event.Stream {
	return &signalingStream{Stream: a.stream, onSub: a.markReady}
}

func (a *testApp) markReady() {
	a.once.Do(func() { close(a.ready) })
}

// signalingStream notifies once a subscriber appears so tests can synchronize with the manifest function.
type signalingStream struct {
	event.Stream
	onSub func()
}

func (s *signalingStream) Sub() <-chan event.Event {
	ch := s.Stream.Sub()
	s.onSub()
	return ch
}

func (s *signalingStream) SubBuf(n int) <-chan event.Event {
	ch := s.Stream.SubBuf(n)
	s.onSub()
	return ch
}

type testExecCtx struct {
	log    *zap.SugaredLogger
	prefix string
	reg    volume.Registry
	app    app.App
}

func (c *testExecCtx) Logger() *zap.SugaredLogger           { return c.log }
func (c *testExecCtx) PathPrefix(override ...string) string { return c.prefix }
func (c *testExecCtx) VolumeRegistry() volume.Registry      { return c.reg }
func (c *testExecCtx) App() app.App                         { return c.app }

type staticRegistry []volume.Volume

func (r staticRegistry) Get(name string) (volume.Volume, bool) {
	for _, v := range r {
		if v.Config().Name == name {
			return v, true
		}
	}
	return nil, false
}

type volumeExecCtx struct {
	log *zap.SugaredLogger
}

func (c volumeExecCtx) Logger() *zap.SugaredLogger { return c.log }
