package acceptance_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DblK/shottower/config"
	openapi "github.com/DblK/shottower/go"
)

func TestMain(m *testing.M) {
	// Speed up the queue polling for the whole acceptance binary. The callback
	// backoff is overridden per test so the production rhythm can be asserted.
	openapi.QueueStartDelay = 2 * time.Millisecond
	openapi.QueuePollInterval = 5 * time.Millisecond
	os.Exit(m.Run())
}

type harness struct {
	t      *testing.T
	svc    openapi.EditAPIServicer
	queue  *openapi.ProcessingQueue
	server *httptest.Server
	client *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	cfg := config.NewShottowerConfig("http://127.0.0.1:4000", config.Stage)
	svc := openapi.NewEditAPIService(cfg)
	q := openapi.NewProcessingQueuer(cfg).(*openapi.ProcessingQueue)

	controller := openapi.NewEditAPIController(svc)
	router := openapi.NewRouter(controller)
	server := httptest.NewServer(router)

	h := &harness{
		t:      t,
		svc:    svc,
		queue:  q,
		server: server,
		client: &http.Client{Timeout: 10 * time.Second},
	}

	t.Cleanup(func() {
		q.Stop()
		server.Close()
	})

	q.StartProcessQueue(svc)
	return h
}

func (h *harness) postRender(rawJSON string) (int, map[string]any) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/stage/render", strings.NewReader(rawJSON))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("post render: %v", err)
	}
	defer res.Body.Close()
	body := decodeMap(h.t, res.Body)
	return res.StatusCode, body
}

func (h *harness) getRender(id string) (int, map[string]any) {
	h.t.Helper()
	res, err := h.client.Get(h.server.URL + "/stage/render/" + id)
	if err != nil {
		h.t.Fatalf("get render: %v", err)
	}
	defer res.Body.Close()
	return res.StatusCode, decodeMap(h.t, res.Body)
}

func decodeMap(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode body %q: %v", string(data), err)
	}
	if m, ok := value.(map[string]any); ok {
		return m
	}
	// Error endpoints encode a bare JSON string.
	return map[string]any{"_raw": string(data)}
}

func renderIDFromPost(t *testing.T, body map[string]any) string {
	t.Helper()
	resp, ok := body["response"].(map[string]any)
	if !ok {
		t.Fatalf("missing response object: %#v", body)
	}
	id, ok := resp["id"].(string)
	if !ok || id == "" {
		t.Fatalf("missing response.id: %#v", body)
	}
	return id
}

func (h *harness) waitForStatus(id string, timeout time.Duration, wanted ...string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		code, body := h.getRender(id)
		if code == http.StatusOK {
			if resp, ok := body["response"].(map[string]any); ok {
				last, _ = resp["status"].(string)
				for _, w := range wanted {
					if last == w {
						return resp
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("render %s did not reach %v (last=%q)", id, wanted, last)
	return nil
}

func (h *harness) waitTerminal(id string, timeout time.Duration) map[string]any {
	return h.waitForStatus(id, timeout, "done", "failed")
}

// ---------- controllable ffmpeg fake ----------

type fakeFFmpeg struct {
	path        string
	logPath     string
	holdPath    string
	releasePath string
	failFlag    string
}

func (f *fakeFFmpeg) callCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "call")
}

func (f *fakeFFmpeg) release() {
	if f.releasePath != "" {
		if err := os.WriteFile(f.releasePath, []byte("go"), 0o600); err != nil {
			panic(err)
		}
	}
}

func (h *harness) fakeOK() *fakeFFmpeg {
	dir := h.t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "@echo off\r\necho call >> \"" + logPath + "\"\r\nexit /b 0\r\n"
	bat := filepath.Join(dir, "ffmpeg.bat")
	if err := os.WriteFile(bat, []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
	old := openapi.FFmpegPath
	openapi.FFmpegPath = bat
	h.t.Cleanup(func() { openapi.FFmpegPath = old })
	return &fakeFFmpeg{path: bat, logPath: logPath}
}

func (h *harness) fakeFailAlways() *fakeFFmpeg {
	dir := h.t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "@echo off\r\necho call >> \"" + logPath + "\"\r\nexit /b 1\r\n"
	bat := filepath.Join(dir, "ffmpeg.bat")
	if err := os.WriteFile(bat, []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
	old := openapi.FFmpegPath
	openapi.FFmpegPath = bat
	h.t.Cleanup(func() { openapi.FFmpegPath = old })
	return &fakeFFmpeg{path: bat, logPath: logPath}
}

func (h *harness) fakeFailFirst() *fakeFFmpeg {
	dir := h.t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	flag := filepath.Join(dir, "fail.flag")
	script := "@echo off\r\necho call >> \"" + logPath + "\"\r\n" +
		"if exist \"" + flag + "\" goto fail\r\n" +
		"exit /b 0\r\n" +
		":fail\r\ndel \"" + flag + "\"\r\nexit /b 1\r\n"
	bat := filepath.Join(dir, "ffmpeg.bat")
	if err := os.WriteFile(bat, []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(flag, []byte("fail"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	old := openapi.FFmpegPath
	openapi.FFmpegPath = bat
	h.t.Cleanup(func() { openapi.FFmpegPath = old })
	return &fakeFFmpeg{path: bat, logPath: logPath, failFlag: flag}
}

func (h *harness) fakeHoldFirst() *fakeFFmpeg {
	dir := h.t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	hold := filepath.Join(dir, "hold.flag")
	release := filepath.Join(dir, "release.flag")
	if err := os.WriteFile(hold, []byte("hold"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	script := "@echo off\r\necho call >> \"" + logPath + "\"\r\n" +
		"if not exist \"" + hold + "\" goto run\r\n" +
		":wait\r\n" +
		"if exist \"" + release + "\" goto donewait\r\n" +
		"ping -n 2 127.0.0.1 >nul\r\n" +
		"goto wait\r\n" +
		":donewait\r\n" +
		"del \"" + hold + "\"\r\n" +
		":run\r\nexit /b 0\r\n"
	bat := filepath.Join(dir, "ffmpeg.bat")
	if err := os.WriteFile(bat, []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
	old := openapi.FFmpegPath
	openapi.FFmpegPath = bat
	h.t.Cleanup(func() { openapi.FFmpegPath = old })
	return &fakeFFmpeg{path: bat, logPath: logPath, holdPath: hold, releasePath: release}
}

// ---------- controllable HTTP servers ----------

type recordedCallback struct {
	mu       sync.Mutex
	payloads []openapi.CallbackResponse
	bodies   []map[string]any
	hits     int
	failLeft int // number of initial requests to fail with 500
}

func newCallbackServer(t *testing.T, failFirst int) (*httptest.Server, *recordedCallback) {
	t.Helper()
	rec := &recordedCallback{failLeft: failFirst}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload openapi.CallbackResponse
		_ = json.Unmarshal(body, &payload)
		var m map[string]any
		_ = json.Unmarshal(body, &m)

		rec.mu.Lock()
		rec.hits++
		rec.payloads = append(rec.payloads, payload)
		rec.bodies = append(rec.bodies, m)
		fail := rec.failLeft > 0
		if fail {
			rec.failLeft--
		}
		rec.mu.Unlock()

		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func (r *recordedCallback) snapshot() (int, []openapi.CallbackResponse) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]openapi.CallbackResponse, len(r.payloads))
	copy(cp, r.payloads)
	return r.hits, cp
}

func (r *recordedCallback) waitHits(t *testing.T, n int, timeout time.Duration) []openapi.CallbackResponse {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		hits, payloads := r.snapshot()
		if hits >= n {
			return payloads
		}
		time.Sleep(10 * time.Millisecond)
	}
	hits, payloads := r.snapshot()
	t.Fatalf("expected %d callback hits, got %d", n, hits)
	return payloads
}

// assetServer serves the given body for /good and 404 for /missing.
func assetServer(t *testing.T, goodBody []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/missing"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "not found")
		case strings.HasPrefix(r.URL.Path, "/slow"):
			time.Sleep(300 * time.Millisecond)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(goodBody)
		default:
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(goodBody)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func imageEditJSON(assetURL, callbackURL string, cache *bool) string {
	cachePart := ""
	if cache != nil {
		cachePart = fmt.Sprintf(`,"cache":%t`, *cache)
	}
	callbackPart := ""
	if callbackURL != "" {
		callbackPart = fmt.Sprintf(`,"callback":%q`, callbackURL)
	}
	return fmt.Sprintf(
		`{"timeline":{"tracks":[{"clips":[{"start":0,"length":2,"asset":{"type":"image","src":%q}}]}]%s},`+
			`"output":{"format":"mp4","resolution":"sd"}%s}`,
		assetURL, cachePart, callbackPart)
}

func emptyEditJSON(callbackURL string) string {
	callbackPart := ""
	if callbackURL != "" {
		callbackPart = fmt.Sprintf(`,"callback":%q`, callbackURL)
	}
	return `{"timeline":{"tracks":[]},"output":{"format":"mp4","resolution":"sd"}` + callbackPart + `}`
}

func boolPtr(b bool) *bool { return &b }
