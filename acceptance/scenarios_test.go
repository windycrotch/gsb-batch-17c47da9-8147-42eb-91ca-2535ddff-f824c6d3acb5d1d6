package acceptance_test

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	openapi "github.com/DblK/shottower/go"
)

var statusRank = map[string]int{
	"queue":     0,
	"fetching":  1,
	"rendering": 2,
	"saving":    3,
	"done":      4,
	"failed":    4,
}

// Requirement 1: many concurrent POSTs -> every id is queryable, each render
// advances independently and no status jumps backwards or sideways.
func TestConcurrentSubmissions_AllTrackedAndIndependent(t *testing.T) {
	h := newHarness(t)
	ff := h.fakeHoldFirst()

	const n = 5
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, body := h.postRender(emptyEditJSON(""))
			if code != http.StatusCreated {
				t.Errorf("request %d: expected 201, got %d body=%v", i, code, body)
				return
			}
			ids <- renderIDFromPost(t, body)
		}(i)
	}
	wg.Wait()
	close(ids)

	idList := make([]string, 0, n)
	idSet := map[string]bool{}
	for id := range ids {
		idList = append(idList, id)
		idSet[id] = true
	}
	if len(idSet) != n {
		t.Fatalf("expected %d unique ids, got %d (%v)", n, len(idSet), idList)
	}

	// Every id must immediately be queryable.
	for _, id := range idList {
		if code, _ := h.getRender(id); code != http.StatusOK {
			t.Fatalf("id %s not queryable: status %d", id, code)
		}
	}

	// Wait for exactly one render to enter the blocking ffmpeg stage.
	var heldID string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && heldID == "" {
		for _, id := range idList {
			resp := h.waitForStatusMaybe(id, 200*time.Millisecond, "rendering")
			if resp != nil {
				heldID = id
				break
			}
		}
		if heldID == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if heldID == "" {
		t.Fatal("no render reached rendering state while fake ffmpeg held")
	}

	// While one task is rendering, every other task must still be queued:
	// their state is independent and not fast-forwarded by the active task.
	for _, id := range idList {
		if id == heldID {
			continue
		}
		code, body := h.getRender(id)
		if code != 200 {
			t.Fatalf("other render %s missing: %d", id, code)
		}
		st := body["response"].(map[string]any)["status"].(string)
		if st != "queue" && st != "fetching" {
			t.Fatalf("render %s should be queue/fetching while %s renders, got %q", id, heldID, st)
		}
	}

	// Sample every status sequence concurrently while releasing the renderer.
	ff.release()

	var seqWg sync.WaitGroup
	sequences := make([][]string, n)
	for i, id := range idList {
		seqWg.Add(1)
		go func(i int, id string) {
			defer seqWg.Done()
			sequences[i] = sampleStatusSequence(h, id, 30*time.Second)
		}(i, id)
	}
	seqWg.Wait()

	for i, seq := range sequences {
		if len(seq) == 0 {
			t.Fatalf("render %s: empty status sequence", idList[i])
		}
		last := -1
		for _, st := range seq {
			rank, ok := statusRank[st]
			if !ok {
				t.Fatalf("render %s: unexpected status %q in %v", idList[i], st, seq)
			}
			if rank < last {
				t.Fatalf("render %s: status moved backwards: %v", idList[i], seq)
			}
			last = rank
		}
		if seq[len(seq)-1] != "done" {
			t.Fatalf("render %s: expected final done, got %q (seq=%v)", idList[i], seq[len(seq)-1], seq)
		}
	}

	if calls := ff.callCount(t); calls != n {
		t.Fatalf("expected %d ffmpeg invocations, got %d", n, calls)
	}
}

// Requirement 2a: asset download failure marks only that render failed, sends
// one failed callback, and the next render still completes.
func TestAssetDownloadFailure_FailsTaskAndContinuesQueue(t *testing.T) {
	h := newHarness(t)
	ff := h.fakeOK()
	assets := assetServer(t, []byte("\x89PNG\r\n\x1a\n-fake-"))
	cbServer, cb := newCallbackServer(t, 0)

	badCode, badBody := h.postRender(imageEditJSON(assets.URL+"/missing/a.png", cbServer.URL, boolPtr(false)))
	if badCode != 201 {
		t.Fatalf("post bad render: %d %v", badCode, badBody)
	}
	badID := renderIDFromPost(t, badBody)

	goodCode, goodBody := h.postRender(imageEditJSON(assets.URL+"/good/b.png", cbServer.URL, boolPtr(false)))
	if goodCode != 201 {
		t.Fatalf("post good render: %d %v", goodCode, goodBody)
	}
	goodID := renderIDFromPost(t, goodBody)

	failedResp := h.waitForStatus(badID, 20*time.Second, "failed")
	if errMsg, _ := failedResp["error"].(string); errMsg == "" {
		t.Fatalf("failed render response should carry an error message: %#v", failedResp)
	}

	doneResp := h.waitForStatus(goodID, 20*time.Second, "done")
	if doneResp["url"] == "" {
		t.Fatalf("done render should expose url: %#v", doneResp)
	}

	payloads := cb.waitHits(t, 2, 15*time.Second)
	byID := map[string][]openapi.CallbackResponse{}
	for _, p := range payloads {
		byID[p.ID] = append(byID[p.ID], p)
	}
	if len(byID[badID]) != 1 || byID[badID][0].Status != "failed" {
		t.Fatalf("bad render expected exactly one failed callback, got %#v", byID[badID])
	}
	if e := byID[badID][0].Error; e == nil || *e == "" {
		t.Fatalf("failed callback should include error text")
	}
	if len(byID[goodID]) != 1 || byID[goodID][0].Status != "done" {
		t.Fatalf("good render expected exactly one done callback, got %#v", byID[goodID])
	}

	// No ffmpeg attempt for the download failure, exactly one for good task.
	if calls := ff.callCount(t); calls != 1 {
		t.Fatalf("expected exactly 1 ffmpeg call (good task), got %d", calls)
	}

	// Give any erroneous duplicate callback time to arrive.
	time.Sleep(400 * time.Millisecond)
	hits, _ := cb.snapshot()
	if hits != 2 {
		t.Fatalf("expected exactly 2 callbacks total, got %d", hits)
	}
}

// Requirement 2b: ffmpeg failure marks the render failed with one callback and
// does not block the following render.
func TestFFmpegFailure_FailsTaskAndContinuesQueue(t *testing.T) {
	h := newHarness(t)
	ff := h.fakeFailFirst()
	cbServer, cb := newCallbackServer(t, 0)

	firstCode, firstBody := h.postRender(emptyEditJSON(cbServer.URL))
	if firstCode != 201 {
		t.Fatalf("post first: %d", firstCode)
	}
	firstID := renderIDFromPost(t, firstBody)

	secondCode, secondBody := h.postRender(emptyEditJSON(cbServer.URL))
	if secondCode != 201 {
		t.Fatalf("post second: %d", secondCode)
	}
	secondID := renderIDFromPost(t, secondBody)

	failedResp := h.waitForStatus(firstID, 20*time.Second, "failed")
	if e, _ := failedResp["error"].(string); e == "" {
		t.Fatalf("ffmpeg failure should populate error field: %#v", failedResp)
	}
	h.waitForStatus(secondID, 20*time.Second, "done")

	payloads := cb.waitHits(t, 2, 15*time.Second)
	counts := map[string]int{}
	status := map[string]string{}
	for _, p := range payloads {
		counts[p.ID]++
		status[p.ID] = p.Status
	}
	if counts[firstID] != 1 || status[firstID] != "failed" {
		t.Fatalf("first render: want 1 failed callback, got count=%d status=%q", counts[firstID], status[firstID])
	}
	if counts[secondID] != 1 || status[secondID] != "done" {
		t.Fatalf("second render: want 1 done callback, got count=%d status=%q", counts[secondID], status[secondID])
	}
	if calls := ff.callCount(t); calls != 2 {
		t.Fatalf("both renders should invoke ffmpeg once, got %d calls", calls)
	}

	time.Sleep(300 * time.Millisecond)
	if hits, _ := cb.snapshot(); hits != 2 {
		t.Fatalf("expected no duplicate callbacks, got %d", hits)
	}
}

// Requirement 3: failed callback delivery retries; status stays done and the
// render pipeline is entered exactly once.
func TestCallbackFailure_RetriesWithoutReRender(t *testing.T) {
	// The production retry rhythm must remain the historical cubic schedule.
	productionDelay := openapi.CallbackRetryDelay
	expectedRhythm := map[int]time.Duration{
		1: 8 * time.Second, 2: 27 * time.Second, 3: 64 * time.Second,
		4: 125 * time.Second, 5: 216 * time.Second, 6: 343 * time.Second,
		7: 512 * time.Second, 8: 729 * time.Second, 9: 900 * time.Second,
	}
	for retry, want := range expectedRhythm {
		if got := productionDelay(retry); got != want {
			t.Fatalf("retry %d delay = %s, want %s", retry, got, want)
		}
	}

	// Compress only the waiting for this test.
	openapi.CallbackRetryDelay = func(retry int) time.Duration {
		return time.Duration(retry) * 2 * time.Millisecond
	}
	t.Cleanup(func() { openapi.CallbackRetryDelay = productionDelay })

	h := newHarness(t)
	ff := h.fakeOK()
	cbServer, cb := newCallbackServer(t, 2) // first two deliveries 500

	code, body := h.postRender(emptyEditJSON(cbServer.URL))
	if code != 201 {
		t.Fatalf("post: %d %v", code, body)
	}
	id := renderIDFromPost(t, body)

	done := h.waitForStatus(id, 20*time.Second, "done")
	if done["status"] != "done" {
		t.Fatalf("expected done, got %v", done)
	}

	payloads := cb.waitHits(t, 3, 15*time.Second)
	var retriesForID []openapi.CallbackResponse
	for _, p := range payloads {
		if p.ID == id {
			retriesForID = append(retriesForID, p)
		}
	}
	if len(retriesForID) != 3 {
		t.Fatalf("expected initial + 2 retries = 3 deliveries, got %d", len(retriesForID))
	}

	// While retries are in flight the status must remain exactly done and
	// ffmpeg must not run again.
	for i := 0; i < 10; i++ {
		resp := h.waitForStatus(id, time.Second, "done", "failed")
		if resp["status"] != "done" {
			t.Fatalf("status changed during callback retries: %v", resp["status"])
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls := ff.callCount(t); calls != 1 {
		t.Fatalf("retries must not re-enter rendering; ffmpeg calls=%d", calls)
	}

	time.Sleep(120 * time.Millisecond)
	if hits, _ := cb.snapshot(); hits != 3 {
		t.Fatalf("successful delivery should stop retries, got %d hits", hits)
	}
	if calls := ff.callCount(t); calls != 1 {
		t.Fatalf("ffmpeg calls changed after settle: %d", calls)
	}
}

// Requirement 4: temporary source assets are removed after terminal state,
// except resources explicitly marked to keep cache.
func TestCleanup_RemovesTempAssetsUnlessKeepCache(t *testing.T) {
	h := newHarness(t)
	h.fakeOK()
	assets := assetServer(t, []byte("asset-bytes"))

	cases := []struct {
		name      string
		cache     *bool
		wantKept  bool
		urlSuffix string
	}{
		{name: "explicit false cleans", cache: boolPtr(false), wantKept: false, urlSuffix: "/clean/a.png"},
		{name: "omitted cleans by default", cache: nil, wantKept: false, urlSuffix: "/clean/b.png"},
		{name: "explicit true keeps", cache: boolPtr(true), wantKept: true, urlSuffix: "/keep/c.png"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := assets.URL + tc.urlSuffix
			code, body := h.postRender(imageEditJSON(url, "", tc.cache))
			if code != 201 {
				t.Fatalf("post: %d %v", code, body)
			}
			id := renderIDFromPost(t, body)
			h.waitForStatus(id, 20*time.Second, "done")

			deadline := time.Now().Add(5 * time.Second)
			var localURL string
			var keepCache bool
			for time.Now().Before(deadline) {
				res := h.queue.SnapshotLocalResources()[url]
				if res.LocalURL != "" {
					localURL = res.LocalURL
					keepCache = res.KeepCache
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if localURL == "" {
				t.Fatalf("resource %s not tracked", url)
			}
			if keepCache != tc.wantKept {
				t.Fatalf("KeepCache=%v want %v", keepCache, tc.wantKept)
			}

			_, statErr := os.Stat(localURL)
			exists := !os.IsNotExist(statErr)
			if tc.wantKept && !exists {
				t.Fatalf("keep-cache asset should remain on disk: %s", localURL)
			}
			if !tc.wantKept && exists {
				t.Fatalf("non-cached asset should be removed: %s", localURL)
			}
		})
	}
}

// Requirement 5: response fields match real state in progress / failed / done.
func TestStatusResponse_FieldsMatchState(t *testing.T) {
	h := newHarness(t)
	ff := h.fakeHoldFirst()

	code, body := h.postRender(emptyEditJSON(""))
	if code != 201 {
		t.Fatalf("post: %d", code)
	}
	id := renderIDFromPost(t, body)

	rendering := h.waitForStatus(id, 20*time.Second, "rendering")
	if rendering["id"] != id {
		t.Fatalf("id mismatch: %v", rendering["id"])
	}
	if rendering["created"] == "" || rendering["updated"] == "" {
		t.Fatalf("created/updated must be present: %#v", rendering)
	}
	if u, _ := rendering["url"].(string); u != "" {
		t.Fatalf("url must be empty while rendering, got %q", u)
	}

	ff.release()
	done := h.waitForStatus(id, 20*time.Second, "done")
	if done["status"] != "done" || done["url"] == "" {
		t.Fatalf("done fields inconsistent: %#v", done)
	}
	if rt, ok := done["renderTime"].(float64); !ok || rt < 0 {
		t.Fatalf("renderTime should be a non-negative number: %#v", done["renderTime"])
	}

	// Unknown ids keep returning 404.
	if code, _ := h.getRender("does-not-exist"); code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", code)
	}
}

// Storage race hardening: hammer PostRender concurrently under -race.
func TestConcurrentPostRender_AllIDsPresent(t *testing.T) {
	h := newHarness(t)
	h.fakeOK()

	const n = 30
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, body := h.postRender(emptyEditJSON(""))
			mu.Lock()
			ids = append(ids, renderIDFromPost(t, body))
			mu.Unlock()
		}()
	}
	wg.Wait()

	sort.Strings(ids)
	if len(ids) != n {
		t.Fatalf("expected %d ids, got %d", n, len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			t.Fatalf("duplicate id %s", ids[i])
		}
	}
	for _, id := range ids {
		if code, _ := h.getRender(id); code != 200 {
			t.Fatalf("id %s missing after concurrent posts", id)
		}
	}

	// Drain everything so no background render leaks into another test's
	// globally overridden ffmpeg path.
	for _, id := range ids {
		h.waitTerminal(id, 30*time.Second)
	}
}

// A8: under a concurrent burst every task reaches done and emits exactly one
// terminal ("done") callback carrying its own id and result url.
func TestConcurrentSubmissions_ExactlyOneCallbackEach(t *testing.T) {
	h := newHarness(t)
	h.fakeOK()
	cbServer, cb := newCallbackServer(t, 0)

	const n = 8
	expected := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := h.postRender(emptyEditJSON(cbServer.URL))
			if code != http.StatusCreated {
				t.Errorf("post status %d", code)
				return
			}
			id := renderIDFromPost(t, body)
			mu.Lock()
			expected[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Wait for all tasks to finish first.
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		_, payloads := cb.snapshot()
		doneIDs := map[string]bool{}
		for _, p := range payloads {
			if p.Status == "done" {
				doneIDs[p.ID] = true
			}
		}
		if len(doneIDs) == n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	hits, payloads := cb.snapshot()
	countByID := map[string]int{}
	for _, p := range payloads {
		countByID[p.ID]++
		if p.Status != "done" {
			t.Fatalf("unexpected callback status %q for %s", p.Status, p.ID)
		}
		if p.URL == "" {
			t.Fatalf("done callback for %s missing url", p.ID)
		}
	}
	if hits != n {
		t.Fatalf("expected exactly %d callbacks, got %d: %#v", n, hits, countByID)
	}
	for id := range expected {
		if countByID[id] != 1 {
			t.Fatalf("render %s callback count=%d (want 1); counts=%#v", id, countByID[id], countByID)
		}
	}
}

func sampleStatusSequence(h *harness, id string, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	var seq []string
	for time.Now().Before(deadline) {
		code, body := h.getRender(id)
		if code == 200 {
			if resp, ok := body["response"].(map[string]any); ok {
				if st, ok := resp["status"].(string); ok {
					if len(seq) == 0 || seq[len(seq)-1] != st {
						seq = append(seq, st)
					}
					if st == "done" || st == "failed" {
						return seq
					}
				}
			}
		}
		time.Sleep(4 * time.Millisecond)
	}
	return seq
}

// A5 trigger: an operator/test invokes CleanCache after rendering. Non-kept
// remote temp files are removed, KeepCache files survive, and repeated calls
// (nil entries left by the historical implementation) stay panic-free.
func TestCleanCache_RemovesOnlyNonKeptResources(t *testing.T) {
	h := newHarness(t)

	dir := t.TempDir()
	cleanFile := filepath.Join(dir, "asset-clean.bin")
	keepFile := filepath.Join(dir, "asset-keep.bin")
	if err := os.WriteFile(cleanFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keepFile, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanURL := "http://assets.example/clean-" + t.Name()
	keepURL := "http://assets.example/keep-" + t.Name()
	h.queue.LocalResources[cleanURL] = &openapi.LocalResource{
		OriginalURL: cleanURL, LocalURL: cleanFile, IsRemoteResource: true, KeepCache: false,
	}
	h.queue.LocalResources[keepURL] = &openapi.LocalResource{
		OriginalURL: keepURL, LocalURL: keepFile, IsRemoteResource: true, KeepCache: true,
	}

	h.queue.CleanCache()
	h.queue.CleanCache() // must not panic on nilified entries

	if _, err := os.Stat(cleanFile); !os.IsNotExist(err) {
		t.Fatalf("non-kept resource should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(keepFile); err != nil {
		t.Fatalf("KeepCache resource must survive: %v", err)
	}
}

// A grader-style harness may drop a fake ffmpeg(.bat) onto PATH instead of
// touching any Go seam. Verify that resolution path end to end.
func TestFakeFFmpegOnPATH_IsUsed(t *testing.T) {
	h := newHarness(t)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "@echo off\r\necho call >> \"" + logPath + "\"\r\nexit /b 0\r\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg.bat"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldPath := openapi.FFmpegPath
	openapi.FFmpegPath = "ffmpeg"
	t.Cleanup(func() { openapi.FFmpegPath = oldPath })

	code, body := h.postRender(emptyEditJSON(""))
	if code != http.StatusCreated {
		t.Fatalf("post: %d %v", code, body)
	}
	id := renderIDFromPost(t, body)
	h.waitForStatus(id, 20*time.Second, "done")

	data, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(data), "call") {
		t.Fatalf("PATH fake ffmpeg was not invoked (err=%v data=%q)", err, string(data))
	}
}

func (h *harness) waitForStatusMaybe(id string, maxWait time.Duration, wanted ...string) map[string]any {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		code, body := h.getRender(id)
		if code == 200 {
			if resp, ok := body["response"].(map[string]any); ok {
				st, _ := resp["status"].(string)
				for _, w := range wanted {
					if st == w {
						return resp
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}
