package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// config holds the server configuration resolved from flags and environment.
type config struct {
	Port        string
	YtDlpPath   string
	MaxSessions int
}

// parseConfig resolves configuration with the same precedence as the existing
// -port/PORT pair: an explicit command-line flag wins, then the environment
// variable, then the built-in default.
func parseConfig(args []string, getenv func(string) string) (config, error) {
	fs := flag.NewFlagSet("yt-dlp-restream", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.String("port", "", "Server port")
	ytdlpPath := fs.String("ytdlp", "", "Path to yt-dlp binary")
	maxSessions := fs.Int("max-sessions", 0, "Maximum concurrent stream sessions")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	cfg := config{
		Port:      firstNonEmpty(*port, getenv("PORT"), "8080"),
		YtDlpPath: firstNonEmpty(*ytdlpPath, getenv("YTDLP_PATH"), "yt-dlp"),
	}
	if setOn(fs, "max-sessions") {
		cfg.MaxSessions = *maxSessions
	} else {
		cfg.MaxSessions = getEnvIntWith(getenv, "MAX_STREAMS", DefaultMaxSessions)
	}
	if cfg.MaxSessions < 1 {
		cfg.MaxSessions = 1
	}
	return cfg, nil
}

func setOn(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func main() {
	cfg, err := parseConfig(os.Args[1:], func(key string) string { return getEnv(key, "") })
	if err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	// Graceful shutdown
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	if err := run(context.Background(), cfg, signals); err != nil {
		log.Fatal(err)
	}
	log.Println("Server stopped")
}

// run starts the server described by cfg, waits for a shutdown signal and then
// shuts down gracefully. It is separated from main so the signal-to-shutdown
// wiring can be exercised in tests with an injected signal channel.
func run(ctx context.Context, cfg config, signals <-chan os.Signal) error {
	ytdlp := &YtDlp{Path: cfg.YtDlpPath}
	sessions := NewSessionManager(cfg.MaxSessions)

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", NewStreamHandler(ytdlp, sessions))
	mux.HandleFunc("/health", HealthHandler)

	server := newServer(cfg.Port, mux)

	errc := make(chan error, 1)
	go func() {
		log.Printf("Starting server on :%s (max sessions: %d)", cfg.Port, sessions.Capacity())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-signals:
		log.Println("Shutting down...")
	case <-ctx.Done():
		log.Println("Context cancelled, shutting down...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return gracefulShutdown(shutdownCtx, server, sessions)
}

// gracefulShutdown cancels in-flight streams (so their subprocesses terminate
// and slots are freed) before draining the HTTP server. Cancelling first keeps
// the drain from waiting on streams that may never finish on their own.
func gracefulShutdown(ctx context.Context, server *http.Server, sessions *SessionManager) error {
	if sessions != nil {
		sessions.Shutdown()
	}
	return server.Shutdown(ctx)
}

// newServer builds the HTTP server with the given handler.
func newServer(port string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // No timeout for streaming responses
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// getEnvIntWith returns the environment variable parsed as an int, or fallback
// when it is unset or not a valid integer.
func getEnvIntWith(getenv func(string) string, key string, fallback int) int {
	value := getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
