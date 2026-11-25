package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/arixbit/xiaomu-desktop/internal/httpx"
)

// SessionConfig configures an isolated loopback stream endpoint (/s/<id>/<filename>)
// with its own headers, optional range-prefetch acceleration and URL refresh callback.
type SessionConfig struct {
	URL      string
	Headers  map[string]string
	Filename string
	Prefetch bool
	Refresh  func(ctx context.Context) (string, map[string]string, error)
}

type sessionHeadersKey struct{}

// ErrSessionRemoved is returned (wrapped in a sessionError) for work that
// races with RemoveSession, so in-flight requests stop instead of fetching
// more upstream chunks for a session that no longer exists.
var ErrSessionRemoved = errors.New("proxy: session removed")

// sessionError carries the HTTP status the player should see. It always
// wraps a cause so errors.Is still works.
type sessionError struct {
	status int
	msg    string
	cause  error
}

func (e *sessionError) Error() string { return e.msg }
func (e *sessionError) Unwrap() error { return e.cause }

func removedError() error {
	return &sessionError{status: http.StatusNotFound, msg: ErrSessionRemoved.Error(), cause: ErrSessionRemoved}
}

func refreshError(cause error) error {
	msg := "proxy: media address refresh failed"
	if cause != nil {
		msg = msg + ": " + cause.Error()
	}
	return &sessionError{status: http.StatusBadGateway, msg: msg, cause: cause}
}

func writeSessionError(w http.ResponseWriter, err error) {
	var se *sessionError
	if errors.As(err, &se) {
		http.Error(w, se.msg, se.status)
		return
	}
	http.Error(w, err.Error(), http.StatusBadGateway)
}

type proxySession struct {
	mu            sync.RWMutex
	targetURL     string
	headers       map[string]string
	filename      string
	prefetch      bool
	refresh       func(ctx context.Context) (string, map[string]string, error)
	lastRefreshed time.Time

	// removed flips once under Proxy.mu in RemoveSession. Every in-flight
	// request selects on removedCh so removal cancels the whole session even
	// while the player keeps its half of the connection open.
	removed    bool
	removedCh  chan struct{}
	removeOnce sync.Once

	// refreshMu guards refreshFlight, the single in-flight refresh generation.
	// A generation is cleared as soon as it completes, so each independent
	// burst of 401/403 requests coalesces into exactly one callback while a
	// later burst (e.g. the new link also expires) can refresh again.
	refreshMu     sync.Mutex
	refreshFlight *refreshFlight
}

type refreshResult struct {
	url     string
	headers map[string]string
	err     error
}

type refreshFlight struct {
	done chan struct{}
	res  refreshResult
}

// markRemoved closes removedCh exactly once. Called while Proxy.mu is held.
func (s *proxySession) markRemoved() {
	s.removeOnce.Do(func() {
		s.mu.Lock()
		s.removed = true
		s.mu.Unlock()
		close(s.removedCh)
	})
}

// alive reports whether the session is still mounted.
func (s *proxySession) alive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.removed
}

// removalOrContextErr classifies a pre-commit upstream error. A dead session
// yields a 404-wrapping removed error; a plain request cancellation (the
// player disconnected but the session still exists) returns the context error
// so callers stay silent instead of writing a misleading 404.
func (s *proxySession) removalOrContextErr(err error) error {
	if !s.alive() {
		return removedError()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// withLifetime derives a context that is cancelled when the request ends or
// the session is removed.
func (s *proxySession) withLifetime(ctx context.Context) (context.Context, context.CancelFunc) {
	lifetimeCtx, cancelLifetime := context.WithCancel(ctx)
	stop := make(chan struct{})
	go func() {
		select {
		case <-s.removedCh:
			cancelLifetime()
		case <-stop:
		}
	}()
	return lifetimeCtx, func() {
		close(stop)
		cancelLifetime()
	}
}

// RegisterSession mounts a dedicated local route that keeps its own headers and
// preserves the filename extension.
func (p *Proxy) RegisterSession(cfg SessionConfig) (id, localURL string) {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	id = hex.EncodeToString(raw[:])

	name := sanitizeFilename(cfg.Filename, cfg.URL)
	s := &proxySession{
		targetURL: strings.TrimSpace(cfg.URL),
		headers:   clone(cfg.Headers),
		filename:  name,
		prefetch:  cfg.Prefetch,
		refresh:   cfg.Refresh,
		removedCh: make(chan struct{}),
	}
	p.mu.Lock()
	if p.sessions == nil {
		p.sessions = map[string]*proxySession{}
	}
	// A random ID collision must never resurrect a removed session.
	if old, exists := p.sessions[id]; exists {
		old.markRemoved()
	}
	p.sessions[id] = s
	p.mu.Unlock()
	return id, p.base + "/s/" + id + "/" + url.PathEscape(name)
}

// RemoveSession unmounts a registered session and cancels every in-flight
// request that belongs to it. A session removed here can never be re-mounted:
// refresh and chunk workers observe removedCh and stop without touching the
// Proxy session table again.
func (p *Proxy) RemoveSession(id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	s, ok := p.sessions[id]
	if ok {
		delete(p.sessions, id)
		s.markRemoved()
	}
	p.mu.Unlock()
}

func (p *Proxy) session(id string) (*proxySession, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s, ok := p.sessions[id]
	return s, ok
}

func (s *proxySession) snapshot() (target string, headers map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targetURL, clone(s.headers)
}

func (s *proxySession) hasRefresh() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.refresh != nil
}

// refreshOnce runs the session refresh callback at most once per concurrent
// burst: the first caller executes the callback while every other caller waits
// for and reuses that same result.
//
// A failed refresh (callback error or empty address) is not cached as session
// state and its flight is released immediately, so a later independent 401 can
// try again; a successful result is stored atomically and becomes the single
// URL/header set the whole session uses afterwards.
//
// The callback runs on a context detached from any individual request so a
// leader whose player disconnects mid-refresh cannot strand the followers.
// Removal always wins: a session removed concurrently is never re-mounted,
// even if the callback resolves successfully afterwards.
func (s *proxySession) refreshOnce(ctx context.Context) (string, map[string]string, error) {
	s.refreshMu.Lock()
	flight := s.refreshFlight
	if flight == nil {
		flight = &refreshFlight{done: make(chan struct{})}
		s.refreshFlight = flight
		go s.runRefresh(ctx, flight)
	}
	s.refreshMu.Unlock()

	select {
	case <-flight.done:
	case <-ctx.Done():
		return "", nil, ctx.Err()
	case <-s.removedCh:
		return "", nil, removedError()
	}

	res := flight.res
	if res.err != nil {
		return "", nil, refreshError(res.err)
	}
	return res.url, clone(res.headers), nil
}

// runRefresh executes one refresh generation and publishes its result. All
// session state mutation happens under s.mu and is skipped when the session
// has been removed, so a late callback cannot re-mount a dead session.
func (s *proxySession) runRefresh(ctx context.Context, flight *refreshFlight) {
	res := refreshResult{}
	defer func() {
		if r := recover(); r != nil {
			res = refreshResult{err: fmt.Errorf("refresh callback panic: %v", r)}
		}
		// Publish the result first, then release the flight slot while still
		// holding refreshMu so the next generation cannot start before the
		// waiters of this one are released.
		s.refreshMu.Lock()
		flight.res = res
		if s.refreshFlight == flight {
			s.refreshFlight = nil
		}
		close(flight.done)
		s.refreshMu.Unlock()
	}()

	refresh := s.refresh
	newURL, newHeaders, err := refresh(context.WithoutCancel(ctx))
	if err != nil || strings.TrimSpace(newURL) == "" {
		if err == nil {
			err = errors.New("refresh callback returned an empty address")
		}
		res = refreshResult{err: err}
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		res = refreshResult{err: ErrSessionRemoved}
		return
	}
	s.targetURL = strings.TrimSpace(newURL)
	if newHeaders != nil {
		s.headers = clone(newHeaders)
	}
	s.lastRefreshed = time.Now()
	res = refreshResult{url: s.targetURL, headers: clone(s.headers)}
}

func (p *Proxy) serveSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	// Path format: /s/<id>/<filename> or /s/<id>/sub?url=<encoded>
	rest := strings.TrimPrefix(r.URL.Path, "/s/")
	id, tail, ok := strings.Cut(rest, "/")
	if !ok || id == "" {
		http.NotFound(w, r)
		return
	}
	sess, found := p.session(id)
	if !found {
		http.NotFound(w, r)
		return
	}

	target, headers := sess.snapshot()
	isSub := tail == "sub"
	if isSub {
		target = r.URL.Query().Get("url")
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, "url must be http or https", http.StatusBadRequest)
		return
	}

	// Tie every upstream call made for this request to the session lifetime:
	// RemoveSession cancels them even if the player stops reading.
	reqCtx, cancelLifetime := sess.withLifetime(r.Context())
	defer cancelLifetime()

	isHLSPath := playlist("", u.Path) || playlist("", sess.filename)
	if r.Method == http.MethodGet && sess.prefetch && !isSub && !isHLSPath {
		if handled, perr := p.servePrefetch(w, r, sess, target, headers, reqCtx); handled {
			if perr != nil && !errors.Is(perr, context.Canceled) {
				// Headers may already be committed; the error only surfaces
				// when the status line has not been written.
				writeSessionError(w, perr)
			}
			return
		}
		// The prefetch path may have performed a single-flight refresh before
		// deciding to fall back (e.g. upstream lacks Range support). Re-snapshot
		// so the direct request never uses the pre-refresh URL or headers.
		if !isSub {
			target, headers = sess.snapshot()
		}
	}

	resp, err := p.doSessionRequest(reqCtx, r.Method, target, headers, r.Header.Get("Range"), isHLSPath)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeSessionError(w, err)
		return
	}
	// Only the main media endpoint owns the refresh callback. HLS sub-resources
	// (/sub) always use the session's current snapshot, which already carries
	// any refreshed URL and headers.
	if !isSub && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && sess.hasRefresh() {
		newURL, newHdr, refreshErr := sess.refreshOnce(reqCtx)
		httpx.DrainAndClose(resp.Body)
		if refreshErr != nil {
			// Never retry with the stale address: surface a hard failure so
			// the player does not mistake a half response for success.
			if errors.Is(refreshErr, context.Canceled) {
				return
			}
			writeSessionError(w, refreshErr)
			return
		}
		target, headers = newURL, newHdr
		resp, err = p.doSessionRequest(reqCtx, r.Method, target, headers, r.Header.Get("Range"), isHLSPath)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			writeSessionError(w, err)
			return
		}
	}
	defer httpx.DrainAndClose(resp.Body)

	base := u
	if resp.Request != nil && resp.Request.URL != nil {
		base = resp.Request.URL
	}
	if r.Method == http.MethodHead {
		copyMediaHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		copyMediaHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	wrapSub := func(raw string) string {
		return p.base + "/s/" + id + "/sub?url=" + url.QueryEscape(raw)
	}
	if playlist(resp.Header.Get("Content-Type"), base.Path) {
		p.writeSessionPlaylist(w, resp, base.String(), headers, wrapSub, reqCtx)
		return
	}
	buf := make([]byte, 64)
	n, readErr := io.ReadFull(resp.Body, buf)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		http.Error(w, readErr.Error(), http.StatusBadGateway)
		return
	}
	if n > 0 && bytes.HasPrefix(bytes.TrimSpace(buf[:n]), []byte("#EXTM3U")) {
		if resp.StatusCode == http.StatusPartialContent {
			p.writeSessionPlaylist(w, resp, base.String(), headers, wrapSub, reqCtx)
			return
		}
		restBody, _, err := httpx.ReadLimit(resp.Body, resp.ContentLength, playlistLimit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		body := append(append([]byte{}, buf[:n]...), restBody...)
		p.writeRewrittenWith(w, base.String(), body, wrapSub)
		return
	}
	copyMediaHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if n > 0 {
		_, _ = w.Write(buf[:n])
	}
	rc := http.NewResponseController(w)
	if resp.Header.Get("Content-Length") == "" && rc.Flush() == nil {
		copyFlushing(w, resp.Body, rc)
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) doSessionRequest(ctx context.Context, method, target string, headers map[string]string, rangeHdr string, isHLS bool) (*http.Response, error) {
	reqCtx := context.WithValue(ctx, sessionHeadersKey{}, headers)
	req, err := http.NewRequestWithContext(reqCtx, method, target, nil)
	if err != nil {
		return nil, err
	}
	applyMap(req, headers)
	if rangeHdr != "" && !isHLS {
		req.Header.Set("Range", rangeHdr)
	}
	return p.do(req)
}

func (p *Proxy) writeSessionPlaylist(w http.ResponseWriter, resp *http.Response, playlistURL string, headers map[string]string, wrap func(string) string, ctx context.Context) {
	if resp.StatusCode == http.StatusPartialContent {
		reqCtx := context.WithValue(ctx, sessionHeadersKey{}, headers)
		req := resp.Request.Clone(reqCtx)
		req.Header.Del("Range")
		applyMap(req, headers)
		httpx.DrainAndClose(resp.Body)
		full, err := p.do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer httpx.DrainAndClose(full.Body)
		if full.StatusCode != http.StatusOK {
			http.Error(w, "upstream did not return a complete playlist", http.StatusBadGateway)
			return
		}
		resp = full
		playlistURL = full.Request.URL.String()
	}
	body, _, err := httpx.ReadLimit(resp.Body, resp.ContentLength, playlistLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	p.writeRewrittenWith(w, playlistURL, body, wrap)
}

func (p *Proxy) writeRewrittenWith(w http.ResponseWriter, playlistURL string, body []byte, wrap func(string) string) {
	if !bytes.HasPrefix(bytes.TrimSpace(body), []byte("#EXTM3U")) {
		http.Error(w, "upstream returned a page instead of an HLS playlist", http.StatusBadGateway)
		return
	}
	out := rewriteM3U8(string(body), playlistURL, wrap)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, out)
}

func applyMap(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		if v == "" || strings.EqualFold(k, "Host") {
			continue
		}
		req.Header.Set(k, v)
	}
}

func sanitizeFilename(filename, rawURL string) string {
	name := strings.TrimSpace(filename)
	if name == "" {
		if u, err := url.Parse(rawURL); err == nil {
			name = path.Base(u.Path)
		}
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		return "stream.mp4"
	}
	return name
}
