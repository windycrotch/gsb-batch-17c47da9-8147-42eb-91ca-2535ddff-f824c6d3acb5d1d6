package main

import (
	"io"
	"log"
	"net/http"
	"net/url"
)

// NewStreamHandler creates an HTTP handler for streaming audio.
func NewStreamHandler(ytdlp *YtDlp) http.HandlerFunc {
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

		log.Printf("Streaming: %s", rawURL)

		result, err := ytdlp.Stream(r.Context(), rawURL)
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

		// For HEAD requests, just return headers
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Stream the audio to the response
		written, err := io.Copy(w, result.Stdout)
		if err != nil {
			log.Printf("Stream interrupted after %d bytes: %v", written, err)
			return
		}

		log.Printf("Stream completed: %d bytes", written)
	}
}
