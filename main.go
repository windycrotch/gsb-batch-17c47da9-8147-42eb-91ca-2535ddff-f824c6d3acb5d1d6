package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	port := flag.String("port", getEnv("PORT", "8080"), "Server port")
	ytdlpPath := flag.String("ytdlp", getEnv("YTDLP_PATH", "yt-dlp"), "Path to yt-dlp binary")
	flag.Parse()

	ytdlp := &YtDlp{Path: *ytdlpPath}

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", NewStreamHandler(ytdlp))
	mux.HandleFunc("/health", HealthHandler)

	server := &http.Server{
		Addr:         ":" + *port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // No timeout for streaming responses
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Starting server on :%s", *port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	<-done
	log.Println("Shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Shutdown error: %v", err)
	}

	log.Println("Server stopped")
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
