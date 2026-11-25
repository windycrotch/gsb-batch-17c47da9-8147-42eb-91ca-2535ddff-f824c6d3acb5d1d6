package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
)

// Streamer abstracts the audio pipeline so handlers can be exercised with
// mocks as well as with the real yt-dlp/ffmpeg pipeline.
type Streamer interface {
	Stream(ctx context.Context, url string) (*StreamResult, error)
}

// NewStreamHandler creates an HTTP handler for streaming audio.
//
// When sessions is non-nil, every request must first acquire a session slot.
// Requests that arrive while the server is at capacity are rejected with 503
// before any subprocess is started. HEAD requests acquire and release a slot
// too, but never launch yt-dlp/ffmpeg.
func NewStreamHandler(streamer Streamer, sessions *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Log incoming request details for debugging
		log.Printf("Request: %s %s", r.Method, r.URL.String())
		if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
			log.Printf("Range header: %s", rangeHeader)
		}

		// Allow HEAD requests for probing
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		rawURL := r.URL.Query().Get("url")
		if rawURL == "" {
			http.Error(w, "Missing 'url' query parameter", http.StatusBadRequest)
			return
		}

		// Basic URL validation
		parsedURL, err := url.Parse(rawURL)
		if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
			http.Error(w, "Invalid URL", http.StatusBadRequest)
			return
		}

		// The session context is a child of the request context, so a client
		// disconnect cancels it; a graceful shutdown cancels it as well.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		// Reserve capacity before starting any subprocess. The slot is released
		// when the handler returns, which covers normal completion, client
		// disconnect and upstream failure alike.
		if sessions != nil {
			release, ok := sessions.Acquire(cancel)
			if !ok {
				log.Printf("Rejecting stream request: server at capacity")
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, BusyMessage+"\n")
				return
			}
			defer release()
		}

		// HEAD is a probe only: it occupies a slot but starts no processes.
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Accept-Ranges", "none")
			w.Header().Set("Cache-Control", "no-cache, no-store")
			w.WriteHeader(http.StatusOK)
			return
		}

		log.Printf("Streaming: %s", rawURL)

		result, err := streamer.Stream(ctx, rawURL)
		if err != nil {
			log.Printf("Failed to start stream: %v", err)
			http.Error(w, "Failed to start stream", http.StatusInternalServerError)
			return
		}
		defer result.Close()

		// Set headers for audio streaming
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Accept-Ranges", "none")
		w.Header().Set("Cache-Control", "no-cache, no-store")

		// Stream the audio to the response
		written, err := io.Copy(w, result.Stdout)
		if err != nil {
			log.Printf("Stream interrupted after %d bytes: %v", written, err)
			return
		}

		log.Printf("Stream completed: %d bytes", written)
	}
}

// NewTestableStreamHandler is kept as an alias of NewStreamHandler for
// existing callers; the Streamer interface is now the native handler input.
func NewTestableStreamHandler(streamer Streamer, sessions *SessionManager) http.HandlerFunc {
	return NewStreamHandler(streamer, sessions)
}
