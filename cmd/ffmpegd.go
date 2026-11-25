package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alfg/ffmpegd/ffmpeg"
	"github.com/gorilla/websocket"
)

const (
	logo = `
███████╗███████╗███╗   ███╗██████╗ ███████╗ ██████╗ ██████╗ 
██╔════╝██╔════╝████╗ ████║██╔══██╗██╔════╝██╔════╝ ██╔══██╗
█████╗  █████╗  ██╔████╔██║██████╔╝█████╗  ██║  ███╗██║  ██║
██╔══╝  ██╔══╝  ██║╚██╔╝██║██╔═══╝ ██╔══╝  ██║   ██║██║  ██║
██║     ██║     ██║ ╚═╝ ██║██║     ███████╗╚██████╔╝██████╔╝
╚═╝     ╚═╝     ╚═╝     ╚═╝╚═╝     ╚══════╝ ╚═════╝ ╚═════╝ 
                                                      v2.1.0
	`
	version     = "ffmpegd version 2.1.0"
	description = "[\u001b[32mffmpegd\u001b[0m] - websocket server for \u001b[33mffmpeg-commander\u001b[0m.\n"
	usage       = `
Usage:
  ffmpegd [--host address] [port]   Run server on localhost:8080 by default.
  ffmpegd version                   Print version.
  ffmpegd help                      This help text.

Options:
  --host address   Address to listen on. Use 0.0.0.0 to let other machines on
                   your network connect. Can also be set with $FFMPEGD_HOST.
	`
	progressInterval = time.Second * 1
	writeTimeout     = time.Second * 10
	maxQueuedJobs    = 100
)

var (
	port = "8080"
	host = "localhost"

	// Sites allowed to connect, besides the daemon's own localhost origin.
	remoteOrigins = []string{
		"https://alfg.github.io",
		"https://alfg.dev",
		"https://ffmpeg-commander.com",
		"https://www.ffmpeg-commander.com",
	}
	allowedOrigins []string

	clients = newHub()

	// defaultServer wires the package-level /ws handler and CLI process to the
	// ownership-aware task lifecycle.
	defaultServer = NewServer(Options{
		QueueSize: maxQueuedJobs,
		Hub:       clients,
		Runner:    realRunner{},
	})
)

// FilesResponse http response for files endpoint.
type FilesResponse struct {
	Cwd     string   `json:"cwd"`
	Folders []string `json:"folders"`
	Files   []file   `json:"files"`
}

type file struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// sendBuffer is the per-connection outbound queue size. A client that does not
// read fast enough is evicted instead of stalling every other sender.
const sendBuffer = 256

// hub is the set of connected clients. It guards only the connection set;
// writes happen on a per-connection writer goroutine so one slow peer cannot
// hold the hub lock across a blocking WriteJSON and wedge unrelated clients or
// the encode dispatcher.
type hub struct {
	mu    sync.Mutex
	conns map[*websocket.Conn]*hubClient
}

type hubClient struct {
	conn  *websocket.Conn
	queue chan any
	done  chan struct{}
	once  sync.Once
}

func newHub() *hub {
	return &hub{conns: make(map[*websocket.Conn]*hubClient)}
}

func (h *hub) add(c *websocket.Conn) {
	hc := &hubClient{
		conn:  c,
		queue: make(chan any, sendBuffer),
		done:  make(chan struct{}),
	}
	h.mu.Lock()
	h.conns[c] = hc
	h.mu.Unlock()
	go h.writeLoop(hc)
}

func (h *hub) writeLoop(hc *hubClient) {
	defer hc.conn.Close()
	for {
		select {
		case <-hc.done:
			return
		case v := <-hc.queue:
			_ = hc.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := hc.conn.WriteJSON(v); err != nil {
				h.remove(hc.conn)
				return
			}
		}
	}
}

func (h *hub) remove(c *websocket.Conn) {
	h.mu.Lock()
	hc := h.conns[c]
	if hc != nil {
		delete(h.conns, c)
	}
	h.mu.Unlock()
	if hc != nil {
		// Unblock the writer and any in-flight write promptly.
		hc.once.Do(func() { close(hc.done) })
		_ = c.Close()
	} else {
		_ = c.Close()
	}
}

// closeAll disconnects every tracked client (server shutdown).
func (h *hub) closeAll() {
	h.mu.Lock()
	clients := make([]*hubClient, 0, len(h.conns))
	for _, hc := range h.conns {
		clients = append(clients, hc)
	}
	h.conns = make(map[*websocket.Conn]*hubClient)
	h.mu.Unlock()
	for _, hc := range clients {
		hc.once.Do(func() { close(hc.done) })
		_ = hc.conn.Close()
	}
}

// send enqueues one message for a client without blocking on the network. A
// full queue evicts the slow consumer.
func (h *hub) send(c *websocket.Conn, v any) {
	h.mu.Lock()
	hc := h.conns[c]
	h.mu.Unlock()
	if hc == nil {
		return
	}
	select {
	case hc.queue <- v:
	case <-hc.done:
	default:
		h.remove(c)
	}
}

// broadcast enqueues a message for every tracked client.
func (h *hub) broadcast(v any) {
	h.mu.Lock()
	clients := make([]*hubClient, 0, len(h.conns))
	for _, hc := range h.conns {
		clients = append(clients, hc)
	}
	h.mu.Unlock()
	for _, hc := range clients {
		h.send(hc.conn, v)
	}
}

func Run() {
	if err := parseArgs(os.Args[1:]); err != nil {
		fmt.Println("\u001b[31m" + err.Error() + "\u001b[0m")
		fmt.Println(usage)
		os.Exit(2)
	}
	allowedOrigins = append([]string{
		"http://localhost:" + port,
		"http://127.0.0.1:" + port,
	}, remoteOrigins...)

	// CLI Banner.
	printBanner()

	// Check if FFmpeg/FFprobe are available.
	err := verifyFFmpeg()
	if err != nil {
		fmt.Println("\u001b[31m" + err.Error() + "\u001b[0m")
		fmt.Println("\u001b[31mPlease ensure FFmpeg and FFprobe are installed and available on $PATH.\u001b[0m")
		os.Exit(1)
	}

	// HTTP/WS Server.
	startServer()
}

func parseArgs(args []string) error {
	if h := os.Getenv("FFMPEGD_HOST"); h != "" {
		host = h
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "version" || arg == "-v" || arg == "--version":
			fmt.Println(version)
			os.Exit(0)
		case arg == "help" || arg == "-h" || arg == "--help":
			fmt.Println(usage)
			os.Exit(0)
		case arg == "--host" || arg == "-host":
			if i+1 == len(args) {
				return errors.New("--host needs an address, such as 0.0.0.0")
			}
			i++
			host = args[i]
		case strings.HasPrefix(arg, "--host="):
			host = strings.TrimPrefix(arg, "--host=")
		default:
			if n, err := strconv.Atoi(arg); err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("unknown argument %q", arg)
			}
			port = arg
		}
	}
	return nil
}

func printBanner() {
	fmt.Println(logo)
	fmt.Print(description + "\n")
}

// listen opens the server's listeners. "localhost" listens on both loopback
// addresses, since browsers may resolve it to either.
func listen() ([]net.Listener, error) {
	if host != "localhost" {
		l, err := net.Listen("tcp", net.JoinHostPort(host, port))
		if err != nil {
			return nil, err
		}
		return []net.Listener{l}, nil
	}

	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, err
	}
	listeners := []net.Listener{l}
	// IPv6 may be unavailable; IPv4 alone is enough.
	if l6, err := net.Listen("tcp", net.JoinHostPort("::1", port)); err == nil {
		listeners = append(listeners, l6)
	}
	return listeners, nil
}

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func startServer() {
	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/files", handleFiles)
	http.Handle("/", http.FileServer(http.Dir("./")))

	listeners, err := listen()
	if err != nil {
		fmt.Println("\u001b[31mCould not start server: " + err.Error() + "\u001b[0m")
		if strings.Contains(err.Error(), "address already in use") {
			fmt.Println("Is ffmpegd already running? To use another port, run: ffmpegd 8081")
		}
		os.Exit(1)
	}

	// Handles queued encodes, one at a time.
	go handleMessages()

	fmt.Println("  Server started on \u001b[33mhttp://" + net.JoinHostPort(host, port) + "\u001b[0m.")
	if !isLoopback(host) {
		fmt.Println("  \u001b[31mListening on " + host + ", so other machines may be able to connect.\u001b[0m")
	}
	fmt.Println("  - Go to \u001b[33mhttps://ffmpeg-commander.com\u001b[0m to connect!")
	fmt.Println("  - \u001b[33mffmpegd\u001b[0m must be enabled in ffmpeg-commander options.")
	fmt.Println("")
	fmt.Printf("Waiting for connection...")

	errs := make(chan error, len(listeners))
	for _, l := range listeners {
		go func(l net.Listener) { errs <- http.Serve(l, nil) }(l)
	}
	fmt.Println("\n\u001b[31mServer stopped: " + (<-errs).Error() + "\u001b[0m")
	os.Exit(1)
}

func isAllowedOrigin(origin string) bool {
	for _, o := range allowedOrigins {
		if origin == o {
			return true
		}
	}
	return false
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	defaultServer.ServeWS(w, r)
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		prefix = "."
	}
	prefix = strings.TrimSuffix(prefix, "/")

	wd, _ := os.Getwd()
	resp := &FilesResponse{
		Cwd:     wd,
		Folders: []string{},
		Files:   []file{},
	}

	files, _ := os.ReadDir(cleanPath(prefix))
	for _, f := range files {
		if f.IsDir() {
			if prefix == "." {
				resp.Folders = append(resp.Folders, f.Name()+"/")
			} else {
				resp.Folders = append(resp.Folders, prefix+"/"+f.Name()+"/")
			}
		} else {
			info, err := f.Info()
			if err != nil {
				continue // Removed since the directory was read.
			}
			obj := file{Name: prefix + "/" + f.Name(), Size: info.Size()}
			if prefix == "." {
				obj.Name = f.Name()
			}
			resp.Files = append(resp.Files, obj)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	cors(w, r)
	json.NewEncoder(w).Encode(resp)
}

func cleanPath(path string) string {
	// Clean path to make safe for use with filepath.Join.
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		path = filepath.Clean(string(os.PathSeparator) + path)
		path, _ = filepath.Rel(string(os.PathSeparator), path)
	}
	return filepath.Clean(path)
}

func cors(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Origin")
	if origin := r.Header.Get("Origin"); isAllowedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
}

// handleMessages starts the default server's serial queue dispatcher. It is
// retained as the historical entrypoint invoked by `go handleMessages()` in
// startServer.
func handleMessages() {
	defaultServer.Start()
}

func verifyFFmpeg() error {
	f := &ffmpeg.FFmpeg{}
	version, err := f.Version()
	if err != nil {
		return err
	}
	fmt.Println("  Checking FFmpeg version....\u001b[32m" + version + "\u001b[0m")

	probe := &ffmpeg.FFProbe{}
	version, err = probe.Version()
	if err != nil {
		return err
	}
	fmt.Println("  Checking FFprobe version...\u001b[32m" + version + "\u001b[0m\n")
	return nil
}

// clearLine erases what's left of a progress line after .
const clearLine = "[K"

// cancelEncode preserves the historical id-less package entrypoint by
// cancelling the default server's currently active task.
func cancelEncode() {
	_, _ = defaultServer.CancelActive("")
}
