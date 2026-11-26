// Package engine downloads one stream (VOD or live) through a single
// pipeline: a feeder, then a worker pool, then an ordered writer. A live
// stream only differs in the feeder, which keeps discovering new segments.
// Everything after the feeder is the same code, so there are no separate VOD
// and live managers.
package engine

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/m314dev/m314dl/internal/bbts"
	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/mp4"
	"github.com/m314dev/m314dl/internal/sampleaes"
)

type Config struct {
	Client    *httpx.Client
	Threads   int      // concurrency ceiling; 0 = auto (up to adaptiveMax)
	Keys      *Keyring // CENC content keys by KID (zero KID = bare key); nil = none
	BBTSKey   []byte   // 16-byte AES key; when set, every .ts segment is BBTS-decrypted
	AdFilters []*regexp.Regexp
	LiveLimit time.Duration   // stop live recording after this long (0 = until end)
	Stop      <-chan struct{} // graceful live stop: feeder exits, pipeline drains, mux proceeds
	FromStart bool            // live: download the whole DVR window instead of starting at the edge
	Progress  *Progress       // shared across streams; may be nil
	Verbose   func(format string, args ...any)
	// PartDir, when set, is where segment part files go instead of beside the
	// output. A stream's output already sits in its download's temp folder, so
	// this matters for a plain file, whose output is the finished file itself.
	PartDir string
	// Verbatim commits segment bytes exactly as received: no fake-image-header
	// strip, no decryption. A plain file's chunks are its own bytes. A PNG, or
	// a chunk that happens to start with 0xFFD8, must not be "de-obfuscated".
	Verbatim bool
	// PaceRealtime slows a Sink restream down to the source's real playback
	// rate. Without it a finite (VOD) source downloads as fast as it can and
	// floods the live output, overrunning a TS viewer's buffer or piling the
	// whole asset into the packager window at once. A live source already paces
	// itself, so leave this off there.
	PaceRealtime bool
	// Sink, when non-nil, receives the ordered, decrypted segments instead of a
	// file (live restreaming). The rest of the download pipeline is the same:
	// the same feeder, workers, decryption and strict in-order commit. So
	// restreaming gets every property the file path has (adaptive concurrency,
	// CENC/AES-128/BBTS decryption, live refresh, hole tolerance).
	Sink Sink
	// Warn reports a live source that has stopped moving: refreshes that keep
	// failing, or a window that keeps listing the same segments. Printed even
	// without -v, since a viewer sees only a frozen stream. Falls back to
	// Verbose when nil.
	Warn func(format string, args ...any)

	fileURL *fileURL // direct file behind an expiring redirect; set by DownloadFile
}

// InitChanger is an optional Sink capability: a sink that can take a new init
// segment mid-stream (the HLS publisher, which lists every init under its own
// name). A sink without it, such as the ffmpeg TS remux, gets only segments
// that decode with the first init; the others, typically a server side ad
// break with its own encoding, are skipped and the stream resumes when the
// first init returns.
type InitChanger interface {
	InitChangeOK() bool
}

// Sink is where one stream's ordered, decrypted segments go. It is the point
// where file output (the default) and live restreaming part ways. The packager
// on the other side turns these calls into a live HLS, DASH or TS output.
type Sink interface {
	// Init hands over the fMP4 initialization segment (the HLS EXT-X-MAP / DASH
	// init payload). Called at most once per init change, before the segments
	// that reference it. TS streams have no init and never call this.
	Init(data []byte) error
	// Segment hands over one decrypted media segment, in strict playback order.
	Segment(info SegmentInfo, data []byte) error
}

// SegmentInfo is the metadata a packager needs for each segment. Seq is the
// source's media sequence number and is for diagnostics only. The packager
// assigns its own output sequence that only ever goes up, so a source that
// rewinds its numbering cannot jam the output.
type SegmentInfo struct {
	Seq           int64
	Duration      float64 // seconds; 0 = unknown
	Discontinuity bool    // source signaled a discontinuity before this segment
}

// RefreshFunc re-fetches the playlist and returns the fresh stream state.
type RefreshFunc func(ctx context.Context) (*manifest.Stream, error)

type item struct {
	idx    int64
	seg    *manifest.Segment
	isInit bool
	url    string
	rng    *manifest.ByteRange
	key    *manifest.Key
	seq    int64
}

// DownloadStream downloads st into outPath as raw media, the segments written
// one after another. refresh is non-nil for live streams.
func DownloadStream(ctx context.Context, cfg Config, st *manifest.Stream, outPath string, refresh RefreshFunc) error {
	// cfg.Threads is the concurrency ceiling; 0 means auto (handled below).
	if cfg.Verbose == nil {
		cfg.Verbose = func(string, ...any) {}
	}
	if cfg.Warn == nil {
		cfg.Warn = cfg.Verbose
	}
	// CENC: decryption happens in this process, with no external mp4decrypt.
	//
	// The init segment decides whether a stream is encrypted; the manifest does
	// not. Packagers do leave out ContentProtection. A stream judged clear on
	// the manifest's word has its encrypted samples copied straight through,
	// which reaches the player as a black picture with working audio, and no
	// error is raised anywhere along the way.
	var dec *cencDecryptor
	var trackID uint32 // the init's sole track; fragments are retagged to it
	switch {
	case st.Init != nil && st.Init.URL != "":
		// fMP4, including fMP4 SAMPLE-AES (which is the cbcs scheme of CENC).
		// The init segment's tenc and schm boxes name the scheme, and the CENC
		// path handles it.
		initSeg, err := fetchInit(ctx, cfg, st)
		if err != nil {
			return err
		}
		d, err := newCencDecryptor(ctx, cfg, st, initSeg)
		if err != nil {
			return err
		}
		dec = d // nil when the init shows the stream is not encrypted
		trackID = mp4.SoleTrackID(initSeg)
	case streamIsCENC(st):
		return fmt.Errorf("stream %s is CENC/DRM-protected; supply the content key with -key KID:KEY", st.ID)
	}

	// Transport-stream SAMPLE-AES (no init) needs a content key up front, like
	// CENC. Fail fast with a clear message instead of downloading every segment
	// and only then failing to decrypt.
	if dec == nil {
		if k := firstSampleAESKey(st); k != nil && !fetchableKeyURI(k.URI) && resolveKey(cfg.Keys.Map(), k.KID) == nil {
			return fmt.Errorf("stream %s uses SAMPLE-AES encryption; supply the content key with -key <hex-key>", st.ID)
		}
	}

	// A stream with a single segment (SegmentBase or a plain file) streams
	// straight to disk. Restreaming skips this path because a sink needs
	// segment boundaries, and this path produces one blob.
	if cfg.Sink == nil && !st.Live && len(st.Segments) == 1 && st.Segments[0].Key == nil && st.Segments[0].Range == nil {
		if err := downloadSingleFile(ctx, cfg, st, outPath); err != nil {
			return err
		}
		os.Remove(statePath(outPath)) // stale checkpoint from a chunked attempt that fell back here
		return nil
	}

	// File output opens the target and loads any resume checkpoint. Restreaming
	// has neither: its segments are short-lived and live starts at the edge, so
	// the state stays at zero and the feeder behaves like a fresh live run.
	var state *resumeState
	var out *os.File
	if cfg.Sink == nil {
		s, f, err := openOutput(outPath)
		if err != nil {
			return err
		}
		state, out = s, f
		defer out.Close()
		// A live stream's listing changes between runs, so its checkpoint
		// names the committed segments; a VOD's static listing resumes
		// positionally and stays keyless. Once a checkpoint carries keys,
		// keep it keyed. A legacy (keyless) one stays keyless: the keys of
		// what a previous version committed are unknowable.
		state.persistKeys = (st.Live || refresh != nil || state.hasKeys) && !state.legacy
		// Resuming: segments already written count as done, so progress and ETA
		// reflect real remaining work instead of restarting from 0/N.
		if cfg.Progress != nil {
			if n := state.NextIdx.Load(); n > 0 {
				cfg.Progress.AddDone(n)
			}
		}
	} else {
		state = &resumeState{}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Adaptive concurrency: start `ceiling` workers, gated by a limiter that the
	// controller resizes. With -t the ceiling is that count and the controller
	// starts right at it, backing off below it only under rate-limit pressure
	// and then climbing back. With no -t the ceiling is adaptiveMax and the
	// controller ramps up from adaptiveStart, hunting for the point where more
	// requests in flight stop helping.
	ceiling := cfg.Threads
	var ctl *controller
	if ceiling > 0 {
		ctl = newFixedController(ceiling) // user pinned -t: hold it, no ramp, no hunting
	} else {
		ceiling = adaptiveMax
		ctl = newController(ceiling) // no -t: auto-tune up to adaptiveMax
	}
	go ctl.run(ctx, cfg.Verbose)
	go func() { <-ctx.Done(); ctl.lim.unblock() }()
	workers := ceiling

	items := make(chan item, workers*2)
	results := make(chan result, workers*2)

	var wg sync.WaitGroup
	kc := &keyCache{client: cfg.Client}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker(ctx, cfg, ctl, outPath, items, results)
		}()
	}
	writerDone := make(chan error, 1)
	go func() {
		var err error
		if cfg.Sink != nil {
			err = sinkWriter(ctx, cfg, st.Live, outPath, state, kc, dec, trackID, results)
		} else {
			err = writer(ctx, cfg, st.Live, out, outPath, state, kc, dec, trackID, results)
		}
		if err != nil {
			// The writer is the only reader of `results`. Once it dies, the
			// workers fill the buffer and stop, the feeder stops on `items`, and
			// nothing below ever returns: the process sits on the same progress
			// line forever with no sockets open. Cancel so both unwind. The
			// caller sees the writer's error (feedErr is then Canceled).
			cancel()
		}
		writerDone <- err
	}()

	feedErr := feed(ctx, cfg, st, refresh, state, items)
	close(items)
	wg.Wait()
	close(results)
	werr := <-writerDone

	if feedErr != nil && !errors.Is(feedErr, context.Canceled) {
		return feedErr
	}
	if werr != nil {
		return werr
	}
	if err := ctx.Err(); err != nil && cfg.LiveLimit == 0 {
		return err
	}
	os.Remove(statePath(outPath))
	return nil
}

// ---- resume state ----

// resumeState is shared by the feeder and the writer, which run side by side
// for the whole download. The writer advances NextIdx and owns Offset; the
// feeder only reads NextIdx, which is why it is atomic. The committed
// segment keys and the source numbering they were recorded under are guarded
// by mu: the writer adds a key on every commit (recordCommit) and saves the
// checkpoint, the feeder consults the keys to tell already-published
// segments from new ones (committed) and tracks the numbering (setMediaSeq,
// clearIDKeys).
//
// The key sets answer the question the plain NextIdx/Offset pair cannot:
// which segments are the bytes already on disk? A live source replaces its
// URLs (a fresh signature) and rewinds its media sequence, and then the
// position of a segment in the listing says nothing about whether it was
// already published. The keys name the content — its manifest identity, and
// its URL with the signature stripped — so the answer survives both.
type resumeState struct {
	NextIdx atomic.Int64
	Offset  int64

	mu       sync.Mutex
	mediaSeq int64               // source numbering the feeder last saw
	idKeys   map[string]struct{} // committed segments' identity keys
	urlKeys  map[string]struct{} // committed segments' (and inits') URL keys
	idOrder  []string            // idKeys in commit order, for the tail bound
	urlOrder []string            // urlKeys in commit order
	// hasKeys: the checkpoint on disk carried keys. persistKeys: save writes
	// them. Keys are persisted for live streams, whose listing changes
	// between runs; a VOD listing is static, so its positional checkpoint
	// (NextIdx alone) already resumes exactly, and writing thousands of keys
	// after every one of its segments would only churn the disk.
	hasKeys, persistKeys bool
	// legacy marks a keyless checkpoint from an older version (or a
	// hand-written one): resume positionally, as before, and keep the
	// checkpoint keyless. The keys of segments committed before this run are
	// unknowable, and a partial key set would be worse than none.
	legacy bool
}

// maxCommittedKeys bounds the committed-key sets (and so the checkpoint) on
// a channel that records forever: 8192 keys is about 13 hours of 6-second
// segments, far beyond any live window a rewind can jump back into.
const maxCommittedKeys = 8192

// resumeSnapshot is the on-disk JSON form. NextIdx and Offset say how much of
// the output is committed; IDKeys and URLKeys say which segments those
// committed items were, so a rerun can recognize them even after the source
// replaced its URLs or rewound its numbering. MediaSeq is the source's
// numbering at save time: a lower numbering on the next run means the saved
// identities may name new content now.
type resumeSnapshot struct {
	NextIdx  int64    `json:"next_idx"`
	Offset   int64    `json:"offset"`
	MediaSeq int64    `json:"media_seq,omitempty"`
	IDKeys   []string `json:"id_keys,omitempty"`
	URLKeys  []string `json:"url_keys,omitempty"`
}

func statePath(outPath string) string { return outPath + ".m314dl-state" }

func keySet(keys []string) map[string]struct{} {
	if len(keys) == 0 {
		return nil
	}
	s := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		s[k] = struct{}{}
	}
	return s
}

func keyList(s map[string]struct{}) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	return out
}

func openOutput(outPath string) (*resumeState, *os.File, error) {
	st := &resumeState{}
	if b, err := os.ReadFile(statePath(outPath)); err == nil {
		var snap resumeSnapshot
		if json.Unmarshal(b, &snap) == nil {
			st.NextIdx.Store(snap.NextIdx)
			st.Offset = snap.Offset
			st.mediaSeq = snap.MediaSeq
			st.idKeys = keySet(snap.IDKeys)
			st.urlKeys = keySet(snap.URLKeys)
			st.idOrder, st.urlOrder = snap.IDKeys, snap.URLKeys
			st.hasKeys = len(snap.IDKeys)+len(snap.URLKeys) > 0
			st.legacy = snap.NextIdx > 0 && !st.hasKeys
		}
	}
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	if fi, err := f.Stat(); err == nil && fi.Size() < st.Offset {
		// state file lies; start over
		st.NextIdx.Store(0)
		st.Offset = 0
		st.mediaSeq = 0
		st.idKeys, st.urlKeys = nil, nil
		st.idOrder, st.urlOrder = nil, nil
		st.hasKeys, st.legacy = false, false
	}
	if err := f.Truncate(st.Offset); err != nil {
		return nil, nil, err
	}
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return nil, nil, err
	}
	return st, f, nil
}

// save writes the resume checkpoint atomically: it writes a temp file and then
// renames it. A process killed without warning in the middle of a write can
// then never leave a half-written state file behind. The rename can fail
// transiently — on Windows the destination cannot be replaced while another
// process (an indexer, a scanner, a watcher) has it open — so a failed
// attempt is retried a few times before giving up to the next commit's save.
// A checkpoint that is a commit or two stale is still consistent: it only
// ever names a prefix of what is on disk, and openOutput truncates to it.
func (s *resumeState) save(outPath string) {
	s.mu.Lock()
	snap := resumeSnapshot{NextIdx: s.NextIdx.Load(), Offset: s.Offset, MediaSeq: s.mediaSeq}
	if s.persistKeys {
		snap.IDKeys = keyList(s.idKeys)
		snap.URLKeys = keyList(s.urlKeys)
	}
	s.mu.Unlock()
	b, _ := json.Marshal(snap)
	tmp := statePath(outPath) + ".tmp"
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(10 * time.Millisecond)
		}
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			continue
		}
		if err := os.Rename(tmp, statePath(outPath)); err == nil {
			return
		}
	}
}

// recordCommit notes a committed item's keys. The writer calls it once the
// item is published (bytes on disk, or handed to the sink), so the committed
// set always names exactly what the output holds, whatever numbering or
// signing the source uses next.
func (s *resumeState) recordCommit(it item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idKeys == nil {
		s.idKeys = map[string]struct{}{}
	}
	if s.urlKeys == nil {
		s.urlKeys = map[string]struct{}{}
	}
	switch {
	case it.isInit:
		s.addURLKey(initKey(it.url, it.rng))
	case it.seg != nil:
		if id := segIDKey(it.seg); id != "" {
			s.addIDKey(id)
		}
		s.addURLKey(segURLKey(it.seg))
	default:
		s.addURLKey(urlKey(it.url) + rangeKey(it.rng))
	}
}

func (s *resumeState) addIDKey(k string) {
	if _, ok := s.idKeys[k]; ok {
		return
	}
	s.idKeys[k] = struct{}{}
	s.idOrder = append(s.idOrder, k)
	s.idKeys, s.idOrder = trimKeys(s.idKeys, s.idOrder)
}

func (s *resumeState) addURLKey(k string) {
	if _, ok := s.urlKeys[k]; ok {
		return
	}
	s.urlKeys[k] = struct{}{}
	s.urlOrder = append(s.urlOrder, k)
	s.urlKeys, s.urlOrder = trimKeys(s.urlKeys, s.urlOrder)
}

// trimKeys keeps the newest maxCommittedKeys of a committed-key set. The
// oldest are the least likely to be listed again: a source only ever
// re-lists what is near its live window.
func trimKeys(set map[string]struct{}, order []string) (map[string]struct{}, []string) {
	if len(order) <= maxCommittedKeys {
		return set, order
	}
	keep := order[len(order)-maxCommittedKeys:]
	set = make(map[string]struct{}, len(keep))
	for _, k := range keep {
		set[k] = struct{}{}
	}
	return set, keep
}

// committed reports whether the identity key or the URL key names a segment
// that was already committed.
func (s *resumeState) committed(id, uk string) (idHit, urlHit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, idHit = s.idKeys[id]
	_, urlHit = s.urlKeys[uk]
	return
}

// committedURL is committed for the URL key alone (init segments have no
// identity).
func (s *resumeState) committedURL(uk string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.urlKeys[uk]
	return ok
}

// committedID is committed for the identity key alone.
func (s *resumeState) committedID(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.idKeys[id]
	return ok
}

// checkpointInfo returns the source numbering the checkpoint's keys were
// recorded under, and whether it carried keys at all.
func (s *resumeState) checkpointInfo() (mediaSeq int64, hasKeys bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mediaSeq, s.hasKeys
}

// setMediaSeq tracks the source numbering the feeder last saw, so the
// checkpoint can tell a rewind that happened while the process was down.
func (s *resumeState) setMediaSeq(seq int64) {
	s.mu.Lock()
	s.mediaSeq = seq
	s.mu.Unlock()
}

// clearIDKeys drops the committed identity keys when the source rewinds its
// numbering: under the new numbering those identities may name new content,
// and keeping them would hide it. URL keys stay — a segment the new window
// lists under the same URL is the same content.
func (s *resumeState) clearIDKeys() {
	s.mu.Lock()
	s.idKeys = map[string]struct{}{}
	s.idOrder = nil
	s.mu.Unlock()
}

// ---- feeder ----

func feed(ctx context.Context, cfg Config, st *manifest.Stream, refresh RefreshFunc, state *resumeState, items chan<- item) error {
	var idx int64
	// Dedup has two layers. The seen maps remember what this run emitted;
	// entries are retired once the commit lands (see the end of push). The
	// state's committed-key sets (seeded from the checkpoint, grown by the
	// writer on every commit) remember everything already published. Each
	// layer comes in the same two flavors: the manifest identity (the HLS
	// media sequence, the DASH number), stable across a re-signed URL but
	// reusable for new content once the source rewinds its numbering; and
	// the URL key (the path, the signature query stripped), which survives
	// a rewind, so a segment the new window lists under the same name is
	// not published twice.
	seenID := map[string]bool{}
	seenURL := map[string]bool{}
	lastInit := ""
	deadline := time.Time{}
	if cfg.LiveLimit > 0 {
		deadline = time.Now().Add(cfg.LiveLimit)
	}

	// Resume. A keyed checkpoint already names the committed segments (the
	// state's key sets were loaded from it), so all the feeder has to do is
	// start the index counter where the writer's NextIdx stands: the first
	// new item then lands exactly where the writer resumes — whether or not
	// the source still lists the same segments in the same order. A keyless
	// checkpoint (a VOD's, an older version's, or a hand-written one) resumes
	// positionally instead: items below NextIdx are dropped in emit, as they
	// always were.
	nextIdx0 := state.NextIdx.Load()
	savedSeq, hasKeys := state.checkpointInfo()
	keyed := nextIdx0 > 0 && hasKeys
	// idUnsafe reports that the source's numbering rewound (just now, or
	// while this process was down), so an identity seen before the rewind
	// must not hide a new segment that reuses the number.
	idUnsafe := false
	if keyed {
		idx = nextIdx0
		if st.MediaSeq < savedSeq {
			cfg.Verbose("live: %s: media sequence went back from %d to %d while away; trusting only content URLs", st.ID, savedSeq, st.MediaSeq)
			idUnsafe = true
			state.clearIDKeys()
		}
		if cfg.Progress != nil {
			// the committed prefix the walk below skips still counts in the total
			cfg.Progress.AddTotal(nextIdx0)
		}
	}
	state.setMediaSeq(st.MediaSeq)
	// replay lasts until the first new item is emitted. While it does, an
	// init the checkpoint already names is not written again: its bytes are
	// in the output from the previous run.
	replay := keyed

	emit := func(it item) bool {
		if it.idx < state.NextIdx.Load() {
			return true // already written in a previous run
		}
		select {
		case items <- it:
			return true
		case <-ctx.Done():
			return false
		}
	}

	push := func(cur *manifest.Stream) (int, bool) {
		// Build the whole batch first (a cheap walk in memory), publish the
		// total, then emit. Emitting blocks on the worker channel and so runs
		// at download speed. Counting there would grow the denominator over the
		// run and make the percentage slide backwards. Counting up front makes
		// the total final before the first progress tick. A live stream grows
		// at every refresh, but live shows elapsed time and a count rather
		// than a percentage, so there is no bar to run backwards.
		fresh := 0
		var batch []item
		for i := range cur.Segments {
			seg := &cur.Segments[i]
			id, uk := segIDKey(seg), segURLKey(seg)
			init := segInit(seg, cur)
			// A segment is a duplicate when this run already emitted it
			// (the seen maps) or any run already committed it (the state's
			// key sets, seeded from the checkpoint). While the identity is
			// trustworthy it alone decides, so two segments that share a
			// path and differ only in the query never collapse into one.
			// After a rewind the URL key is the only name the content still
			// has.
			var dup bool
			cid, cuk := state.committed(id, uk)
			if id != "" && !idUnsafe {
				dup = seenID[id] || cid
			} else {
				dup = seenURL[uk] || cuk || (id != "" && (seenID[id] || cid))
			}
			if dup {
				continue
			}
			if id != "" {
				seenID[id] = true
			}
			seenURL[uk] = true
			if skipAd(cfg.AdFilters, seg.URL) {
				cfg.Verbose("ad-skip: %s", seg.URL)
				continue
			}
			fresh++
			if init != nil && init.URL != lastInit {
				lastInit = init.URL
				if !replay || !state.committedURL(initKey(init.URL, init.Range)) {
					batch = append(batch, item{idx: idx, isInit: true, url: init.URL, rng: init.Range})
					idx++
					replay = false
				}
			}
			batch = append(batch, item{idx: idx, seg: seg, url: seg.URL, rng: seg.Range, key: seg.Key, seq: seg.Seq})
			idx++
			replay = false
		}
		if cfg.Progress != nil {
			cfg.Progress.AddTotal(int64(len(batch)))
		}
		for i := range batch {
			if !emit(batch[i]) {
				return fresh, false
			}
		}
		// Retire emitted keys the committed set now covers. The seen maps
		// exist to bridge the gap between emission and commit: the feeder
		// runs ahead of the writer, so a segment the source stops listing
		// must stay remembered until its commit lands in the state's key
		// sets — after that the state remembers it (bounded by its tail
		// limit) and the entry here is redundant. What is left behind is
		// only what can never be committed: ad-skipped and failed segments,
		// and the live-edge backlog, all bounded in practice.
		for k := range seenID {
			if state.committedID(k) {
				delete(seenID, k)
			}
		}
		for k := range seenURL {
			if state.committedURL(k) {
				delete(seenURL, k)
			}
		}
		return fresh, true
	}

	if st.Live && refresh != nil && !cfg.FromStart && state.NextIdx.Load() == 0 {
		// start at the live edge: mark the DVR backlog as seen, keep the last 3
		keep := 3
		if n := len(st.Segments); n > keep {
			for i := 0; i < n-keep; i++ {
				seg := &st.Segments[i]
				if id := segIDKey(seg); id != "" {
					seenID[id] = true
				}
				seenURL[segURLKey(seg)] = true
			}
		}
	}
	if _, ok := push(st); !ok {
		return ctx.Err()
	}
	if !st.Live || refresh == nil {
		return nil
	}

	// live loop
	interval := st.Refresh
	if interval <= 0 {
		interval = 4 * time.Second
	}
	// A live source that stops moving is worth a line on the console even
	// without -v. Either the refresh keeps failing, or it succeeds and keeps
	// listing the same segments (a manifest the server never updates, or one
	// the CDN hands back from cache). Say which, once every 30 seconds.
	lastNew, lastWarn := time.Now(), time.Time{}
	lastMediaSeq := st.MediaSeq
	stalled := func(format string, args ...any) {
		if time.Since(lastWarn) < 30*time.Second {
			cfg.Verbose(format, args...)
			return
		}
		lastWarn = time.Now()
		cfg.Warn(format, args...)
	}
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			cfg.Verbose("live: recording limit reached")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cfg.Stop:
			cfg.Verbose("live: stop requested, finishing up")
			return nil
		case <-time.After(interval):
		}
		cur, err := refresh(ctx)
		if err != nil {
			stalled("live: %s: refresh failed (will keep trying): %v", st.ID, err)
			continue
		}
		// A playlist whose numbering went backwards comes from a packager that
		// started over. Its identities now name different content, so identity
		// keys recorded under the old numbering must not hide the new
		// segments — forget them, here and in the checkpoint. URL keys stay:
		// a segment the new window lists under the same URL is the same
		// content and must not be published twice.
		if cur.MediaSeq < lastMediaSeq {
			cfg.Verbose("live: %s: media sequence went back from %d to %d; forgetting identity keys, keeping content URLs", st.ID, lastMediaSeq, cur.MediaSeq)
			seenID = map[string]bool{}
			idUnsafe = true
			state.clearIDKeys()
		}
		lastMediaSeq = cur.MediaSeq
		state.setMediaSeq(cur.MediaSeq)
		fresh, ok := push(cur)
		if !ok {
			return ctx.Err()
		}
		if fresh > 0 {
			lastNew = time.Now()
		} else if since := time.Since(lastNew); since > 30*time.Second {
			last := ""
			if n := len(cur.Segments); n > 0 {
				last = path.Base(strings.SplitN(cur.Segments[n-1].URL, "?", 2)[0])
			}
			stalled("live: %s: no new segments for %s, source still lists %d ending at %s",
				st.ID, since.Round(time.Second), len(cur.Segments), last)
		}
		if !cur.Live {
			cfg.Verbose("live: stream ended")
			return nil
		}
		if cur.Refresh > 0 {
			interval = cur.Refresh
		}
	}
}

func segInit(seg *manifest.Segment, st *manifest.Stream) *manifest.InitMap {
	if seg.Init != nil {
		return seg.Init
	}
	return st.Init
}

// segIDKey is the segment's manifest identity (the HLS media sequence, the
// DASH number or presentation time), or "" when the format gives none. It is
// stable across a re-signed URL, but a source that rewinds its numbering can
// hand the same identity to new content.
func segIDKey(seg *manifest.Segment) string {
	if seg.Identity == "" {
		return ""
	}
	return seg.Identity + rangeKey(seg.Range)
}

// segURLKey names a segment by its URL with the signature query stripped.
// Formats that give no identity dedup by this, and every segment falls back
// to it once the numbering rewinds: a CDN that re-signs its URLs hands the
// same segment out under a new query string, and the path is what names the
// content.
func segURLKey(seg *manifest.Segment) string {
	return urlKey(seg.URL) + rangeKey(seg.Range)
}

// initKey names an init segment the same way segURLKey names a segment.
func initKey(u string, r *manifest.ByteRange) string {
	return urlKey(u) + rangeKey(r)
}

// urlKey drops the query and fragment, which is where CDN signatures live.
func urlKey(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

// dedupKey is the item's content-stable name, used to key its part file: the
// manifest identity when there is one, else the signature-stripped URL.
func (it item) dedupKey() string {
	if it.isInit {
		return "init:" + initKey(it.url, it.rng)
	}
	if it.seg != nil {
		if id := segIDKey(it.seg); id != "" {
			return "id:" + id
		}
		return "url:" + segURLKey(it.seg)
	}
	return "url:" + urlKey(it.url) + rangeKey(it.rng)
}

func rangeKey(r *manifest.ByteRange) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("@%d-%d", r.Start, r.End)
}

func skipAd(filters []*regexp.Regexp, u string) bool {
	for _, re := range filters {
		if re.MatchString(u) {
			return true
		}
	}
	return false
}

// ---- workers ----

type result struct {
	it       item
	err      error
	pressure int
}

// worker streams each segment to its part file. It does not decrypt and does
// not keep the segment in memory; the writer decrypts and assembles in order.
//
// Restreaming keeps the part file too, even though its writer reads the whole
// thing straight back into RAM to decrypt it. Holding those bytes in memory
// instead was tried and reverted. The pipeline is 64 workers deep with a
// results channel twice that size. That depth costs nothing when it is counted
// in file handles, and 19 GB in one process when it is counted in segments.
// Point the spool at a tmpfs and the writes never reach a disk anyway, with a
// size cap the kernel enforces, which is the part an in-process buffer was
// missing.
func worker(ctx context.Context, cfg Config, ctl *controller, outPath string, items <-chan item, results chan<- result) {
	for it := range items {
		if err := ctl.lim.acquire(ctx); err != nil {
			return
		}
		if cfg.Progress != nil {
			cfg.Progress.AddConns(1)
		}
		pressure, err := downloadSegment(ctx, cfg, ctl, it, partPathFor(cfg.PartDir, outPath, it))
		if cfg.Progress != nil {
			cfg.Progress.AddConns(-1)
		}
		ctl.lim.release()
		if pressure > 0 {
			ctl.addPressure(pressure)
		}
		if err != nil {
			ctl.addErr()
		}
		select {
		case results <- result{it: it, err: err, pressure: pressure}:
		case <-ctx.Done():
			return
		}
	}
}

// decryptSegment applies the same in-place transforms the old inline path
// did, in the same order: strip the fake image header, then CENC, then
// AES-128, then BBTS. trackID is the fMP4 init's only track (0 for TS, or for
// an init with several tracks).
func decryptSegment(ctx context.Context, kc *keyCache, dec *cencDecryptor, trackID uint32, bbtsKey []byte, keys map[[16]byte][]byte, it item, data []byte) ([]byte, error) {
	// AES-128 encrypts the whole segment, so before decrypting there is no TS
	// sync byte or MP4 box to find, only random-looking bytes. Ciphertext that
	// happened to start with FF D8 (JPEG) and to hold three 0x47 bytes 188
	// apart was once cut to a length that was not a multiple of the block size
	// (3isk/grzcdn seg 188). So strip after decrypting instead.
	aes128 := it.key != nil && it.key.Method == manifest.EncAES128
	if !aes128 {
		data = stripFakeImageHeader(data)
	}
	// fMP4 CENC and fMP4 SAMPLE-AES (which is cbcs) both decrypt through the
	// CENC path. The init's tenc and schm boxes already chose the scheme when
	// dec was built.
	if !it.isInit && dec != nil && it.key != nil &&
		(it.key.Method == manifest.EncCENC || it.key.Method == manifest.EncSampleAES) {
		if err := dec.decrypt(data); err != nil {
			return nil, fmt.Errorf("CENC decrypt: %w", err)
		}
		// The samples are now plaintext. Drop the senc, saiz and saio boxes so
		// nothing downstream treats the fragment as still encrypted.
		data = mp4.StripFragmentProtection(data)
	}
	// Transport-stream SAMPLE-AES (no fMP4 init): decrypt the elementary
	// streams the way Apple's spec describes and write out a clean TS.
	if !it.isInit && dec == nil && it.key != nil && it.key.Method == manifest.EncSampleAES {
		key, err := sampleAESKey(ctx, kc, keys, it.key)
		if err != nil {
			return nil, err
		}
		iv := it.key.IV
		if iv == nil {
			iv = make([]byte, 16)
			binary.BigEndian.PutUint64(iv[8:], uint64(it.seq))
		}
		out, err := sampleaes.Decrypt(data, key, iv)
		if err != nil {
			return nil, fmt.Errorf("SAMPLE-AES decrypt: %w", err)
		}
		data = out
	}
	if aes128 {
		key, err := kc.get(ctx, it.key.URI)
		if err != nil {
			return nil, fmt.Errorf("fetch AES key: %w", err)
		}
		iv := it.key.IV
		if iv == nil {
			iv = make([]byte, 16)
			binary.BigEndian.PutUint64(iv[8:], uint64(it.seq))
		}
		data, err = decryptAES128CBC(data, key, iv)
		if err != nil {
			return nil, err
		}
		data = stripFakeImageHeader(data)
	}
	// BBTS: an iQiyi-style cipher over the whole TS segment. The key is supplied
	// separately; the IV is carried in each segment's own SDT table. Media
	// segments only, never the init.
	if len(bbtsKey) > 0 && !it.isInit {
		out, err := bbts.Decrypt(data, bbtsKey)
		if err != nil {
			return nil, fmt.Errorf("BBTS decrypt: %w", err)
		}
		data = out
	}
	// CENC init: remove its protection (rename encv to avc1, drop sinf and
	// pssh) so the muxed or raw output is not marked encrypted while its
	// samples are now plaintext.
	if it.isInit && dec != nil {
		data = mp4.SanitizeInit(data)
	}
	// fMP4 media: make the fragment agree with its init. A Smooth Streaming
	// source has a synthesized init, so its PIFF fragments must be re-tagged
	// with the init's track id and given the tfdt (decode time) box they lack.
	// A packaged DASH or HLS fragment already matches and passes through
	// unchanged. This runs after the decrypt, which needs the original moof
	// offsets.
	if !it.isInit && trackID != 0 {
		data = mp4.NormalizeFragment(data, trackID)
	}
	return data, nil
}

// stripFakeImageHeader removes the fake PNG, JPEG, GIF or BMP header some CDNs
// put in front of TS segments as a crude disguise. The real TS or fMP4 payload
// starts at the first 0x47 sync byte or at the first 'ftyp', 'styp' or 'moof'
// box.
func stripFakeImageHeader(b []byte) []byte {
	if len(b) < 8 {
		return b
	}
	isPNG := b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G'
	isJPG := b[0] == 0xFF && b[1] == 0xD8
	isGIF := b[0] == 'G' && b[1] == 'I' && b[2] == 'F'
	isBMP := b[0] == 'B' && b[1] == 'M'
	if !isPNG && !isJPG && !isGIF && !isBMP {
		return b
	}
	// TS: scan for 3 consecutive sync bytes 188 apart
	for i := 0; i < len(b)-188*2-1; i++ {
		if b[i] == 0x47 && b[i+188] == 0x47 && b[i+376] == 0x47 {
			return b[i:]
		}
	}
	// fMP4: box header
	for i := 4; i < len(b)-8; i++ {
		tag := string(b[i : i+4])
		if tag == "ftyp" || tag == "styp" || tag == "moof" || tag == "sidx" {
			return b[i-4:]
		}
	}
	return b
}

// ---- AES-128 ----

type keyCache struct {
	client *httpx.Client
	mu     sync.Mutex
	keys   map[string][]byte
}

func (kc *keyCache) get(ctx context.Context, uri string) ([]byte, error) {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	if kc.keys == nil {
		kc.keys = map[string][]byte{}
	}
	if k, ok := kc.keys[uri]; ok {
		return k, nil
	}
	var key []byte
	if strings.HasPrefix(uri, "data:") {
		i := strings.Index(uri, ",")
		if i < 0 {
			return nil, fmt.Errorf("bad data: key URI")
		}
		payload := uri[i+1:]
		if strings.Contains(uri[:i], "base64") {
			b, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				return nil, err
			}
			key = b
		} else {
			key = []byte(payload)
		}
	} else {
		b, _, err := kc.client.FetchBytes(ctx, uri, "")
		if err != nil {
			return nil, err
		}
		key = b
	}
	if len(key) != 16 {
		return nil, fmt.Errorf("AES-128 key is %d bytes, want 16", len(key))
	}
	kc.keys[uri] = key
	return key, nil
}

func decryptAES128CBC(data, key, iv []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted segment length %d not a multiple of 16", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	// Remove the PKCS7 padding. Be tolerant: leave the data as it is when the
	// padding looks invalid.
	pad := int(out[len(out)-1])
	if pad >= 1 && pad <= 16 && pad <= len(out) {
		valid := true
		for _, p := range out[len(out)-pad:] {
			if int(p) != pad {
				valid = false
				break
			}
		}
		if valid {
			out = out[:len(out)-pad]
		}
	}
	return out, nil
}

// ---- ordered writer ----

// pend is a segment that has been downloaded but not yet committed: either an
// item whose part file is on disk, or a hole in a live stream (a failed
// segment that was skipped).
type pend struct {
	it   item
	hole bool
}

// commitInOrder drains results and commits segments in strict index order. It
// is the one place that handles ordering and live holes, shared by the file
// writer and the restream sink writer. The two differ only in `commit` (what
// to do with a ready segment) and `afterCommit` (bookkeeping after each
// segment; may be nil). It returns the first fatal error, or a missing-segments
// error when the stream ended with gaps that a non-live run cannot accept.
func commitInOrder(cfg Config, live bool, state *resumeState, results <-chan result,
	commit func(idx int64, p pend) error, afterCommit func(next int64)) error {
	pending := map[int64]pend{}
	var failed error
	for r := range results {
		if r.err != nil {
			if failed == nil {
				failed = fmt.Errorf("segment %d (%s): %w", r.it.idx, r.it.url, r.err)
			}
			// A stopped run cancels every fetch in flight at once. Dozens of
			// "segment failed: context canceled" lines would be noise, since
			// nothing really failed.
			if !errors.Is(r.err, context.Canceled) {
				cfg.Verbose("segment failed: %s: %v", r.it.url, r.err)
			}
			if live {
				// live: the segment is gone; a hole keeps the stream moving
				pending[r.it.idx] = pend{hole: true}
			}
			// VOD: no hole. The resume index must not move past a failed
			// segment, or a resume would silently skip it.
			continue
		}
		pending[r.it.idx] = pend{it: r.it}
		for {
			idx := state.NextIdx.Load()
			p, ok := pending[idx]
			if !ok {
				break
			}
			delete(pending, idx)
			if err := commit(idx, p); err != nil {
				return err
			}
			next := state.NextIdx.Add(1)
			if cfg.Progress != nil {
				cfg.Progress.AddDone(1)
			}
			if afterCommit != nil {
				afterCommit(next)
			}
		}
	}
	// an index still pending means that segment failed to download
	if len(pending) > 0 && failed == nil {
		failed = fmt.Errorf("%d segments missing at end of stream", len(pending))
	}
	return failed
}

// writer commits segments to a single output file with resume checkpointing.
func writer(ctx context.Context, cfg Config, live bool, out *os.File, outPath string, state *resumeState, kc *keyCache, dec *cencDecryptor, trackID uint32, results <-chan result) error {
	// commit decrypts one segment's part file and appends it to the output.
	// ponytail: reads the whole part file into memory to decrypt it (CENC and
	// AES need the full segment); peak memory is one segment instead of one per
	// worker. A streaming copy for clear segments could cut that to one buffer
	// if a huge clear segment ever turns up as a real problem.
	commit := func(idx int64, p pend) error {
		if p.hole {
			return nil
		}
		data, err := readDecrypt(ctx, cfg, kc, dec, trackID, outPath, idx, p.it)
		if err != nil {
			return err
		}
		if _, err := out.Write(data); err != nil {
			return err
		}
		state.Offset += int64(len(data))
		state.recordCommit(p.it)
		return nil
	}
	// Checkpoint after every commit. This process may be killed without warning
	// (a TUI in raw mode turns Ctrl-C into a keypress instead of a signal), so
	// the state file must match what is on disk before the kill happens. A
	// stale offset would make openOutput truncate committed data and download
	// it again. The uncommitted tail still resumes from its part files, so a
	// kill costs at most the one segment being written. The write is atomic and
	// tiny. fsync stays periodic; it is there to survive power loss.
	afterCommit := func(next int64) {
		state.save(outPath)
		if next%20 == 0 {
			out.Sync()
		}
	}
	err := commitInOrder(cfg, live, state, results, commit, afterCommit)
	out.Sync()
	state.save(outPath)
	return err
}

// sinkWriter commits segments to a Sink (live restreaming) instead of a file:
// same ordering and hole tolerance, no output file, no resume checkpoint.
func sinkWriter(ctx context.Context, cfg Config, live bool, outPath string, state *resumeState, kc *keyCache, dec *cencDecryptor, trackID uint32, results <-chan result) error {
	var (
		paceStart  time.Time     // wall clock when realtime pacing began
		mediaClock time.Duration // media time emitted since pacing began
		emitted    int           // segments emitted (for the prime burst)
		firstInit  []byte        // the init the stream started with
		skipping   bool          // inside a run of segments the sink cannot take
	)
	canChange := false
	if ic, ok := cfg.Sink.(InitChanger); ok {
		canChange = ic.InitChangeOK()
	}
	commit := func(idx int64, p pend) error {
		if p.hole {
			return nil
		}
		data, err := readDecrypt(ctx, cfg, kc, dec, trackID, outPath, idx, p.it)
		if err != nil {
			return err
		}
		if p.it.isInit {
			first := firstInit == nil
			if first {
				firstInit = data
			}
			same := first || bytes.Equal(data, firstInit)
			if !same && !canChange {
				if !skipping {
					cfg.Verbose("live: the source switched to a different init (an ad break?); this output cannot follow, skipping until it switches back")
				}
				skipping = true
				return nil
			}
			skipping = false
			// Fragments are retagged to their init's track id, so follow the
			// init that is now in force.
			if id := mp4.SoleTrackID(data); id != 0 {
				trackID = id
			}
			if same && !first && !canChange {
				return nil // the sink still holds this init
			}
			return cfg.Sink.Init(data)
		}
		if skipping {
			return nil
		}
		info := SegmentInfo{Seq: p.it.seq}
		if p.it.seg != nil {
			info.Duration = p.it.seg.Duration
			info.Discontinuity = p.it.seg.Discontinuity
		}
		if err := cfg.Sink.Segment(info, data); err != nil {
			return err
		}
		// Pace a finite restream to real time: send a short burst first so
		// players can start without waiting, then hold each further segment
		// for its own duration. Because segments are emitted in order here,
		// the sleep slows the whole pipeline down to the source's natural rate.
		// The download stops racing ahead, so the live output never floods. A
		// live source is already paced; skip it.
		if cfg.PaceRealtime && info.Duration > 0 {
			emitted++
			if emitted <= pacePrimeSegments {
				return nil
			}
			if paceStart.IsZero() {
				paceStart = time.Now()
			}
			mediaClock += time.Duration(info.Duration * float64(time.Second))
			if d := time.Until(paceStart.Add(mediaClock)); d > 0 {
				timer := time.NewTimer(d)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
			}
		}
		return nil
	}
	// sinkWriter commits segments to a Sink (live restreaming) instead of a
	// file: same ordering and hole tolerance, no output file, no resume
	// checkpoint. The committed keys are still recorded: the feeder's dedup
	// reads them to keep a replaced or rewound source from republishing.
	return commitInOrder(cfg, live, state, results, func(idx int64, p pend) error {
		err := commit(idx, p)
		if err == nil && !p.hole {
			state.recordCommit(p.it)
		}
		return err
	}, nil)
}

// pacePrimeSegments is how many segments a paced restream sends back to back
// before slowing to real time. A player that joins then fills its buffer at
// once instead of waiting one segment duration per segment before playback
// starts.
const pacePrimeSegments = 3

// readDecrypt reads a committed segment's part file, decrypts it in place and
// deletes the part file. Both writers use it. The part file is deleted even
// when the decrypt fails. Otherwise a worker killed and restarted on a bad
// stream would leave its segments behind, and 1430 of those directories once
// took up 43 GB that nobody could see.
func readDecrypt(ctx context.Context, cfg Config, kc *keyCache, dec *cencDecryptor, trackID uint32, outPath string, idx int64, it item) ([]byte, error) {
	part := partPathFor(cfg.PartDir, outPath, it)
	data, err := os.ReadFile(part)
	if err != nil {
		return nil, fmt.Errorf("read segment %d: %w", idx, err)
	}
	defer os.Remove(part)
	if cfg.Verbatim {
		return data, nil
	}
	return decryptSegment(ctx, kc, dec, trackID, cfg.BBTSKey, cfg.Keys.Map(), it, data)
}

// ---- plain file ----

// DownloadFile downloads a plain file. Its stream is either one whole-object
// segment (a single connection) or consecutive byte-range chunks of the object
// (parallel and resumable, through the ordinary segment pipeline). A server
// that passed the range probe but then answers a chunk with the whole object is
// caught by the worker (ErrRangeIgnored), and the download restarts on one
// connection, resuming from whatever unbroken prefix was already committed.
func DownloadFile(ctx context.Context, cfg Config, st *manifest.Stream, outPath string) error {
	if cfg.Verbose == nil {
		cfg.Verbose = func(string, ...any) {}
	}
	cfg.Verbatim = true
	cfg.AdFilters = nil // a URL filter meant for ad segments must not blank out the file
	// Already finished: full size on disk and no checkpoint. Without this check
	// a rerun (or a playlist rerun over done titles) finds no checkpoint, so
	// openOutput truncates and the whole file downloads again.
	if r := st.Segments[len(st.Segments)-1].Range; r != nil {
		if fi, err := os.Stat(outPath); err == nil && fi.Size() == r.End+1 {
			if _, err := os.Stat(statePath(outPath)); errors.Is(err, os.ErrNotExist) {
				cfg.Verbose("%s already complete", outPath)
				if cfg.Progress != nil {
					cfg.Progress.AddTotal(1)
					cfg.Progress.AddDone(1)
				}
				return nil
			}
		}
	}
	u := st.Segments[0].URL
	if st.PlaylistURL != "" && st.PlaylistURL != u {
		cfg.fileURL = &fileURL{origin: st.PlaylistURL, cur: u}
	}
	err := DownloadStream(ctx, cfg, st, outPath, nil)
	if !errors.Is(err, ErrRangeIgnored) {
		return err
	}
	cfg.Verbose("server ignored byte ranges; downloading on one connection")
	if cfg.fileURL != nil {
		u = cfg.fileURL.get()
	}
	single := *st
	single.Segments = []manifest.Segment{{URL: u}}
	return DownloadStream(ctx, cfg, &single, outPath, nil)
}

// fileURL is a direct file's current download URL, which can be resolved again
// from the URL the user gave. IPTV panels (Xtream Codes, XUI) and hosts with
// signed redirects send us to a token URL that expires soon. A long chunked
// download outlives it, and its later chunks are refused (406, 403).
// Requesting the origin again gives a fresh token.
type fileURL struct {
	origin string
	mu     sync.Mutex
	cur    string
}

func (f *fileURL) get() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur
}

// refresh replaces stale with a freshly resolved URL. Workers refused on the
// same stale URL share one re-resolve: the lock is held while the request
// runs, so the others wait for it and then find cur already changed.
func (f *fileURL) refresh(ctx context.Context, c *httpx.Client, stale string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cur != stale {
		return nil
	}
	resp, err := c.Open(ctx, f.origin, "bytes=0-0")
	if err != nil {
		return err
	}
	resp.Body.Close()
	f.cur = httpx.FinalURL(resp, f.origin)
	return nil
}

// ---- single big file ----

// downloadSingleFile streams one URL to disk, resuming from the bytes already
// on disk.
func downloadSingleFile(ctx context.Context, cfg Config, st *manifest.Stream, outPath string) error {
	u := st.Segments[0].URL
	var offset int64
	if fi, err := os.Stat(outPath); err == nil {
		offset = fi.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		return nil // already complete
	case resp.StatusCode == http.StatusOK:
		offset = 0 // server ignored Range; start over
	case resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode >= 400:
		return &httpx.StatusError{Code: resp.StatusCode, URL: u}
	}
	if cfg.Progress != nil {
		cfg.Progress.AddTotal(1)
		if resp.ContentLength > 0 {
			cfg.Progress.SetKnownBytes(offset + resp.ContentLength)
		}
	}
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(offset); err != nil {
		return err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	bp := copyBufs.Get().(*[]byte)
	defer copyBufs.Put(bp)
	buf := *bp
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			if cfg.Progress != nil {
				cfg.Progress.AddBytes(int64(n))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read body: %w", rerr)
		}
	}
	if cfg.Progress != nil {
		cfg.Progress.AddDone(1)
	}
	return nil
}

// TempDir is the folder one download stages its files in: raw streams, part
// files and the resume checkpoint. It sits beside the output as <name>.m314dl,
// so two downloads into one directory never share it, and a rerun of the same
// command finds it again and resumes. With root set (-temp-dir) the folder
// moves under root, and a short hash of the output's absolute path keeps two
// outputs that share a name but not a directory apart.
func TempDir(outPath, root string) string {
	base := strings.TrimSuffix(filepath.Base(outPath), filepath.Ext(outPath))
	if root == "" {
		return filepath.Join(filepath.Dir(outPath), base+".m314dl")
	}
	abs, err := filepath.Abs(outPath)
	if err != nil {
		abs = outPath
	}
	h := fnv.New32a()
	h.Write([]byte(abs))
	return filepath.Join(root, fmt.Sprintf("%s-%08x.m314dl", base, h.Sum32()))
}

// TempStreamPath is the raw file one stream downloads to inside its download's
// TempDir: <type>-<id><ext>, for example video-1.mp4 or subtitles-en.rawsub.
// Stream ids repeat across manifests ("video=3000000", "1"), so the folder is
// what keeps concurrent downloads apart; the file name alone could not.
func TempStreamPath(dir string, st *manifest.Stream, ext string) string {
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", st.Type, sanitize(st.ID), ext))
}

var badChars = regexp.MustCompile(`[^\w.\-]+`)

func sanitize(s string) string {
	s = badChars.ReplaceAllString(s, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
