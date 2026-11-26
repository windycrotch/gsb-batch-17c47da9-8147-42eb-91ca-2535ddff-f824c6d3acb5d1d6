package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m314dev/m314dl/internal/manifest"
)

// A live source whose CDN re-signs the segment URLs (new query, same content)
// must not republish what it already sent. These segments carry no manifest
// Identity — the formats where dedup falls back to the URL — so the signature
// in the query must not count as part of the segment's name.
func TestLiveResignedSourceKeepsDedup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", r.URL.Path) // same path = same content, whatever the query
	}))
	defer srv.Close()

	segs := func(n int, sig string, live bool) *manifest.Stream {
		st := &manifest.Stream{ID: "t", Type: manifest.Video, Live: live, Refresh: time.Millisecond}
		for i := 0; i < n; i++ {
			st.Segments = append(st.Segments, manifest.Segment{
				URL: fmt.Sprintf("%s/seg%d?sig=%s", srv.URL, i, sig), Seq: int64(i), Duration: 0.01,
			})
		}
		return st
	}
	var calls atomic.Int64
	refresh := func(context.Context) (*manifest.Stream, error) {
		if calls.Add(1) == 1 {
			return segs(5, "b", true), nil // re-signed: same 0-2 under new URLs, plus 3-4
		}
		return segs(5, "b", false), nil // ENDLIST
	}

	sink := &recordingSink{}
	err := DownloadStream(context.Background(),
		Config{Client: mustClient(t), Threads: 4, Sink: sink, FromStart: true},
		segs(3, "a", true), filepath.Join(t.TempDir(), "o"), refresh)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"[/seg0]", "[/seg1]", "[/seg2]", "[/seg3]", "[/seg4]"}
	if !reflect.DeepEqual(sink.body, want) {
		t.Fatalf("delivered %v, want %v (re-signed URLs must not republish)", sink.body, want)
	}
}

// The packager restarts and its media sequence rewinds, with new content
// mixed into the window under numbers already used. Segments the new window
// lists under the same URL are the same content and must not be published
// twice; segments under old numbers but new URLs are new content and must
// not be hidden by the old numbering.
func TestLiveMediaSeqRollback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	seg := func(path string, seq int64) manifest.Segment {
		return manifest.Segment{
			URL: srv.URL + path, Seq: seq, Duration: 0.01,
			Identity: "seq:" + strconv.FormatInt(seq, 10),
		}
	}
	// before the rewind: seq 102-104 committed
	before := &manifest.Stream{ID: "t", Type: manifest.Video, Live: true, MediaSeq: 102, Refresh: time.Millisecond}
	for i, p := range []string{"/a102", "/a103", "/a104"} {
		before.Segments = append(before.Segments, seg(p, int64(102+i)))
	}
	// after: numbering back to 100; /a102 is the same content, the rest is new
	after := func(live bool) *manifest.Stream {
		st := &manifest.Stream{ID: "t", Type: manifest.Video, Live: live, MediaSeq: 100, Refresh: time.Millisecond}
		for i, p := range []string{"/n100", "/n101", "/a102", "/n103", "/n104"} {
			st.Segments = append(st.Segments, seg(p, int64(100+i)))
		}
		return st
	}
	var calls atomic.Int64
	refresh := func(context.Context) (*manifest.Stream, error) {
		if calls.Add(1) == 1 {
			return after(true), nil
		}
		return after(false), nil
	}

	sink := &recordingSink{}
	err := DownloadStream(context.Background(),
		Config{Client: mustClient(t), Threads: 4, Sink: sink, FromStart: true},
		before, filepath.Join(t.TempDir(), "o"), refresh)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"[/a102]", "[/a103]", "[/a104]", "[/n100]", "[/n101]", "[/n103]", "[/n104]"}
	if !reflect.DeepEqual(sink.body, want) {
		t.Fatalf("delivered %v, want %v", sink.body, want)
	}
}

// rollbackStages is a local origin that plays the whole acceptance script:
// segments 100-102 download normally, the playlist is then replaced with a
// freshly signed one (same content, new query) that extends the window, and
// finally the media sequence rewinds with new content mixed in.
type rollbackStages struct {
	srv *httptest.Server
}

func newRollbackStages(t *testing.T) *rollbackStages {
	r := &rollbackStages{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		fmt.Fprintf(w, "[%s]", rq.URL.Path)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rollbackStages) seg(path string, seq int64, sig string) manifest.Segment {
	return manifest.Segment{
		URL: fmt.Sprintf("%s%s?sig=%s", r.srv.URL, path, sig),
		Seq: seq, Duration: 0.01,
		Identity: "seq:" + strconv.FormatInt(seq, 10),
	}
}

// stage 0: the playlist the download starts with
func (r *rollbackStages) stage0() *manifest.Stream {
	st := &manifest.Stream{ID: "t", Type: manifest.Video, Live: true, MediaSeq: 100, Refresh: time.Millisecond}
	st.Segments = []manifest.Segment{r.seg("/s100", 100, "a"), r.seg("/s101", 101, "a"), r.seg("/s102", 102, "a")}
	return st
}

// stage 1: re-signed (sig=b), window extended to seq 101-105
func (r *rollbackStages) stage1(live bool) *manifest.Stream {
	st := &manifest.Stream{ID: "t", Type: manifest.Video, Live: live, MediaSeq: 101, Refresh: time.Millisecond}
	for i, p := range []string{"/s101", "/s102", "/s103", "/s104", "/s105"} {
		st.Segments = append(st.Segments, r.seg(p, int64(101+i), "b"))
	}
	return st
}

// stage 2: the numbering rewound to 100; /s100 and /s101 are the same
// content (re-signed again), /x102-/x104 are new content
func (r *rollbackStages) stage2(live bool) *manifest.Stream {
	st := &manifest.Stream{ID: "t", Type: manifest.Video, Live: live, MediaSeq: 100, Refresh: time.Millisecond}
	for i, p := range []string{"/s100", "/s101", "/x102", "/x103", "/x104"} {
		st.Segments = append(st.Segments, r.seg(p, int64(100+i), "c"))
	}
	return st
}

// reference is what a single uninterrupted run through stages 0-2 commits.
func (r *rollbackStages) reference() string {
	return "[/s100][/s101][/s102][/s103][/s104][/s105][/x102][/x103][/x104]"
}

// The one-shot reference run: normal download, source replaced with a
// re-signed playlist, then a media-sequence rewind with new content mixed
// in. Nothing is republished, nothing is lost.
func TestLiveReplaceRollbackOneShot(t *testing.T) {
	r := newRollbackStages(t)
	var calls atomic.Int64
	refresh := func(context.Context) (*manifest.Stream, error) {
		switch calls.Add(1) {
		case 1:
			return r.stage1(true), nil
		case 2:
			return r.stage2(true), nil
		default:
			return r.stage2(false), nil // ENDLIST
		}
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	if err := DownloadStream(context.Background(),
		Config{Client: mustClient(t), Threads: 4, FromStart: true},
		r.stage0(), out, refresh); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if string(b) != r.reference() {
		t.Fatalf("one-shot output = %q, want %q", b, r.reference())
	}
	if _, err := os.Stat(out + ".m314dl-state"); !os.IsNotExist(err) {
		t.Fatal("state file should be removed on success")
	}
}

// The same script, but the process is killed (context cancelled with no
// cleanup, like a hard kill) after the re-signed stage committed, and the
// rewind happens while it is down. The rerun must resume from the checkpoint
// and end with bytes identical to the one-shot run: committed segments are
// not written twice, the rewind's new segments are not skipped, and the
// checkpoint is cleaned up on success.
func TestLiveKillRerunByteIdentical(t *testing.T) {
	r := newRollbackStages(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out.ts")

	// run 1: stages 0-1, then the source keeps listing stage 1 until the kill
	ctx, cancel := context.WithCancel(context.Background())
	refresh1 := func(context.Context) (*manifest.Stream, error) {
		return r.stage1(true), nil
	}
	done := make(chan error, 1)
	go func() {
		done <- DownloadStream(ctx, Config{Client: mustClient(t), Threads: 4, FromStart: true}, r.stage0(), out, refresh1)
	}()
	// wait until the checkpoint says all six segments of stages 0-1 committed
	deadline := time.Now().Add(60 * time.Second)
	for {
		var snap resumeSnapshot
		if b, err := os.ReadFile(out + ".m314dl-state"); err == nil && json.Unmarshal(b, &snap) == nil && snap.NextIdx >= 6 {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(out + ".m314dl-state")
			fi, _ := os.Stat(out)
			cancel()
			select {
			case err := <-done:
				t.Fatalf("run 1 returned early: %v (state %s)", err, b)
			case <-time.After(10 * time.Second):
			}
			sz := int64(-1)
			if fi != nil {
				sz = fi.Size()
			}
			t.Fatalf("checkpoint never reached 6 committed segments (state %s, out %d bytes)", b, sz)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel() // the hard kill: no cleanup, the checkpoint is as-of-last-commit
	<-done

	// run 2: the source rewound while we were down; resume and finish
	var calls2 atomic.Int64
	refresh2 := func(context.Context) (*manifest.Stream, error) {
		if calls2.Add(1) == 1 {
			return r.stage2(true), nil
		}
		return r.stage2(false), nil
	}
	if err := DownloadStream(context.Background(),
		Config{Client: mustClient(t), Threads: 4, FromStart: true},
		r.stage2(true), out, refresh2); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if string(b) != r.reference() {
		t.Fatalf("killed+resumed output = %q, want one-shot %q", b, r.reference())
	}
	if _, err := os.Stat(out + ".m314dl-state"); !os.IsNotExist(err) {
		t.Fatal("state file should be removed on success")
	}
}

// A VOD interrupted after its first segments committed leaves a checkpoint;
// the rerun skips exactly the committed segments and completes the file.
func TestVODInterruptedResume(t *testing.T) {
	gate := make(chan struct{})
	var hits sync.Map
	var gen atomic.Int64 // download run, so a late run-1 request can't leak into run 2's log
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Store(fmt.Sprintf("%d:%s", gen.Load(), r.URL.Path), true)
		if r.URL.Path != "/seg0" && r.URL.Path != "/seg1" {
			<-gate // hold the tail until the test kills the first run
		}
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 5; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	out := filepath.Join(t.TempDir(), "out.ts")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- DownloadStream(ctx, Config{Client: mustClient(t), Threads: 2}, st, out, nil)
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var snap resumeSnapshot
		if b, err := os.ReadFile(out + ".m314dl-state"); err == nil && json.Unmarshal(b, &snap) == nil && snap.NextIdx >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("checkpoint never reached 2 committed segments")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	close(gate)
	gen.Store(1) // run 2 must not touch the committed segments

	if err := DownloadStream(context.Background(), Config{Client: mustClient(t), Threads: 2}, st, out, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if string(b) != "[/seg0][/seg1][/seg2][/seg3][/seg4]" {
		t.Fatalf("resumed output = %q", b)
	}
	if _, ok := hits.Load("1:/seg0"); ok {
		t.Fatal("seg0 re-downloaded on resume")
	}
	if _, ok := hits.Load("1:/seg1"); ok {
		t.Fatal("seg1 re-downloaded on resume")
	}
	if _, err := os.Stat(out + ".m314dl-state"); !os.IsNotExist(err) {
		t.Fatal("state file should be removed on success")
	}
}
