package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/arixbit/xiaomu-desktop/internal/httpx"
)

const (
	prefetchChunkSize = 1 << 20 // 1 MiB per ranged upstream chunk
	prefetchWorkers   = 3       // bounded concurrency to avoid rate-limit bans
	prefetchWindow    = 6       // max in-flight/buffered chunks (~6 MiB cap per stream)
)

// servePrefetch accelerates large non-HLS range streams (such as cloud-drive 4K MP4/MKV
// files throttled per connection) using bounded concurrent range reads.
//
// It returns handled=true when the request belongs to the prefetch path. A
// non-nil error is only returned while response headers have not been committed
// yet, so the caller can still render a proper failure status; once streaming
// has started, errors abort the stream silently. handled=false means the
// request shape is unsupported and the caller must proxy it directly (after
// re-snapshotting the session, since a refresh may have changed the URL).
func (p *Proxy) servePrefetch(w http.ResponseWriter, r *http.Request, sess *proxySession, target string, headers map[string]string, reqCtx context.Context) (handled bool, preCommitErr error) {
	if !sess.alive() {
		return true, removedError()
	}
	reqStart, reqEnd, hasRange, ok := parseSingleByteRange(r.Header.Get("Range"))
	if !ok {
		return false, nil
	}
	if !hasRange {
		reqStart, reqEnd = 0, -1
	}
	// Small explicit probes (e.g. player reading initial 64 KiB header or trailer) do not need chunking.
	if reqEnd >= 0 && (reqEnd-reqStart+1) <= prefetchChunkSize {
		return false, nil
	}

	firstEnd := reqStart + prefetchChunkSize - 1
	if reqEnd >= 0 && reqEnd < firstEnd {
		firstEnd = reqEnd
	}
	firstRange := fmt.Sprintf("bytes=%d-%d", reqStart, firstEnd)
	resp, err := p.doSessionRequest(reqCtx, http.MethodGet, target, headers, firstRange, false)
	if err != nil {
		if serr := sess.removalOrContextErr(err); serr != nil {
			return true, serr
		}
		// A transport error before any refresh: let the direct path retry once.
		return false, nil
	}
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && sess.hasRefresh() {
		newURL, newHdr, refreshErr := sess.refreshOnce(reqCtx)
		httpx.DrainAndClose(resp.Body)
		if refreshErr != nil {
			// No stale-link fallback: a failed refresh is a hard failure.
			return true, refreshErr
		}
		target, headers = newURL, newHdr
		resp, err = p.doSessionRequest(reqCtx, http.MethodGet, target, headers, firstRange, false)
		if err != nil {
			if serr := sess.removalOrContextErr(err); serr != nil {
				return true, serr
			}
			return true, err
		}
	}
	if resp.StatusCode != http.StatusPartialContent {
		httpx.DrainAndClose(resp.Body)
		// Either range requests are unsupported, or a refreshed upstream
		// answered differently. The caller re-snapshots the session before
		// falling back, so it never reuses a pre-refresh URL.
		return false, nil
	}
	_, actualEnd, total, validCR := parseContentRange(resp.Header.Get("Content-Range"))
	if !validCR || total <= 0 {
		httpx.DrainAndClose(resp.Body)
		return false, nil
	}

	// Read first chunk completely before committing headers so errors can still surface as a status.
	firstData, _, err := httpx.ReadLimit(resp.Body, firstEnd-reqStart+1, prefetchChunkSize)
	ct := resp.Header.Get("Content-Type")
	httpx.DrainAndClose(resp.Body)
	if err != nil {
		if serr := sess.removalOrContextErr(err); serr != nil {
			return true, serr
		}
		return true, err
	}
	if int64(len(firstData)) != firstEnd-reqStart+1 {
		return true, fmt.Errorf("upstream first chunk returned %d bytes, want %d", len(firstData), firstEnd-reqStart+1)
	}

	finalEnd := total - 1
	if reqEnd >= 0 && reqEnd < finalEnd {
		finalEnd = reqEnd
	}
	if finalEnd < reqStart {
		return false, nil
	}

	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(finalEnd-reqStart+1, 10))
	if hasRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", reqStart, finalEnd, total))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	if _, err := w.Write(firstData); err != nil {
		return true, nil
	}
	nextOffset := actualEnd + 1
	if nextOffset > finalEnd {
		return true, nil
	}

	// reqCtx is already bound to the session lifetime, so RemoveSession
	// cancels every in-flight chunk through streamCtx.
	streamCtx, cancel := context.WithCancel(reqCtx)
	defer cancel()

	type chunkRange struct {
		start int64
		end   int64
	}
	type chunkSlot struct {
		done chan struct{}
		data []byte
		err  error
	}

	jobs := make(chan struct {
		r    chunkRange
		slot *chunkSlot
	}, prefetchWindow)

	var wg sync.WaitGroup
	for range prefetchWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if !sess.alive() {
					job.slot.err = removedError()
					close(job.slot.done)
					cancel()
					continue
				}
				// Snapshot per chunk: after a single-flight refresh every
				// remaining chunk uses the new URL and headers.
				curTarget, curHeaders := sess.snapshot()
				data, fetchErr := p.fetchChunk(streamCtx, sess, curTarget, curHeaders, job.r.start, job.r.end)
				job.slot.data, job.slot.err = data, fetchErr
				close(job.slot.done)
				if fetchErr != nil {
					cancel()
				}
			}
		}()
	}

	ordered := make(chan *chunkSlot, prefetchWindow)
	go func() {
		defer close(jobs)
		defer close(ordered)
		for off := nextOffset; off <= finalEnd; off += prefetchChunkSize {
			end := min(off+prefetchChunkSize-1, finalEnd)
			slot := &chunkSlot{done: make(chan struct{})}
			select {
			case <-streamCtx.Done():
				return
			case ordered <- slot:
			}
			select {
			case <-streamCtx.Done():
				slot.err = streamCtx.Err()
				close(slot.done)
				return
			case jobs <- struct {
				r    chunkRange
				slot *chunkSlot
			}{r: chunkRange{start: off, end: end}, slot: slot}:
			}
		}
	}()

	for slot := range ordered {
		select {
		case <-streamCtx.Done():
			cancel()
			wg.Wait()
			return true, nil
		case <-slot.done:
		}
		if slot.err != nil || streamCtx.Err() != nil {
			cancel()
			wg.Wait()
			// Headers are already committed; abort the stream rather than
			// splice bytes from a failed or mixed-URL chunk.
			return true, nil
		}
		if len(slot.data) > 0 {
			if _, err := w.Write(slot.data); err != nil {
				cancel()
				wg.Wait()
				return true, nil
			}
		}
	}
	wg.Wait()
	return true, nil
}

func (p *Proxy) fetchChunk(ctx context.Context, sess *proxySession, target string, headers map[string]string, start, end int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !sess.alive() {
		return nil, removedError()
	}
	rng := fmt.Sprintf("bytes=%d-%d", start, end)
	resp, err := p.doSessionRequest(ctx, http.MethodGet, target, headers, rng, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if sess.hasRefresh() {
			newURL, newHdr, refreshErr := sess.refreshOnce(ctx)
			httpx.DrainAndClose(resp.Body)
			if refreshErr != nil {
				return nil, refreshErr
			}
			// Retry this one chunk against the refreshed address only, so a
			// chunk can never concatenate bytes from two different links.
			target, headers = newURL, newHdr
			resp, err = p.doSessionRequest(ctx, http.MethodGet, target, headers, rng, false)
			if err != nil {
				return nil, err
			}
		}
	}
	defer httpx.DrainAndClose(resp.Body)
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("upstream chunk HTTP %d", resp.StatusCode)
	}
	expected := end - start + 1
	data, exceeded, err := httpx.ReadLimit(resp.Body, expected, expected)
	if err != nil {
		return nil, err
	}
	// Reject short bodies and Range-ignoring 200 replies: either would splice
	// bytes belonging to a different offset into the stream.
	if exceeded || int64(len(data)) != expected {
		return nil, fmt.Errorf("upstream chunk bytes %d-%d returned %d bytes, want %d", start, end, len(data), expected)
	}
	return data, nil
}

// parseSingleByteRange parses an HTTP Range header like "bytes=0-" or "bytes=100-200".
// Multi-range or suffix-range headers return ok=false so callers fall back to direct proxy.
func parseSingleByteRange(hdr string) (start, end int64, hasRange, ok bool) {
	hdr = strings.TrimSpace(hdr)
	if hdr == "" {
		return 0, -1, false, true
	}
	if !strings.HasPrefix(strings.ToLower(hdr), "bytes=") {
		return 0, 0, false, false
	}
	spec := strings.TrimSpace(hdr[len("bytes="):])
	if strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	sStr, eStr, found := strings.Cut(spec, "-")
	if !found || strings.TrimSpace(sStr) == "" {
		return 0, 0, false, false
	}
	s, err := strconv.ParseInt(strings.TrimSpace(sStr), 10, 64)
	if err != nil || s < 0 {
		return 0, 0, false, false
	}
	e := int64(-1)
	if strings.TrimSpace(eStr) != "" {
		e, err = strconv.ParseInt(strings.TrimSpace(eStr), 10, 64)
		if err != nil || e < s {
			return 0, 0, false, false
		}
	}
	return s, e, true, true
}

func parseContentRange(hdr string) (start, end, total int64, ok bool) {
	hdr = strings.TrimSpace(hdr)
	if !strings.HasPrefix(strings.ToLower(hdr), "bytes ") {
		return 0, 0, 0, false
	}
	rest := strings.TrimSpace(hdr[len("bytes "):])
	span, totalStr, found := strings.Cut(rest, "/")
	if !found || totalStr == "*" {
		return 0, 0, 0, false
	}
	sStr, eStr, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, 0, false
	}
	s, err1 := strconv.ParseInt(strings.TrimSpace(sStr), 10, 64)
	e, err2 := strconv.ParseInt(strings.TrimSpace(eStr), 10, 64)
	t, err3 := strconv.ParseInt(strings.TrimSpace(totalStr), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || s < 0 || e < s || t <= e {
		return 0, 0, 0, false
	}
	return s, e, t, true
}
