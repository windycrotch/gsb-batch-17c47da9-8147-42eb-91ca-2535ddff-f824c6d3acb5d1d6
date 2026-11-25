package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: airplay-helper serve|stop ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "stop":
		fs := flag.NewFlagSet("stop", flag.ExitOnError)
		out := fs.String("out", "", "output dir of the instance to stop")
		fs.Parse(os.Args[2:])
		if *out == "" {
			fmt.Fprintln(os.Stderr, "missing required flag: -out")
			os.Exit(2)
		}
		pidfile := filepath.Join(*out, "helper.pid")
		if err := TakeOver(pidfile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// TakeOver waited for the old instance to die, and the old instance
		// swept nothing itself: its teardown saw the ownership go. So the
		// sweep is ours — unless a NEW cast planted its pidfile while we
		// waited, in which case the directory is theirs, files included.
		if pid, err := readPidfile(pidfile); err == nil && processAlive(pid) {
			return
		}
		cleanOutDir(*out)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runServe(argv []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c JobConfig
	var parent int
	var ipOverride, rangeHint string
	var idleSec float64
	fs.StringVar(&c.Source, "source", "", "media file")
	fs.StringVar(&c.OutDir, "out", "", "HLS output dir")
	fs.StringVar(&c.FFmpeg, "ffmpeg", "", "ffmpeg binary")
	fs.IntVar(&parent, "parent", 0, "IINA pid to watch")
	fs.StringVar(&ipOverride, "ip", "", "LAN IPv4 address to advertise (auto-detected by default)")
	fs.Float64Var(&c.Duration, "duration", 0, "source duration seconds")
	fs.StringVar(&c.VCodec, "vcodec", "", "source video codec (mpv name)")
	fs.StringVar(&c.ACodec, "acodec", "", "source audio codec (mpv name)")
	fs.IntVar(&c.VDepth, "vdepth", 0, "source video bit depth (0 = unknown)")
	fs.IntVar(&c.VHeight, "vheight", 0, "source video height in pixels (0 = unknown)")
	fs.IntVar(&c.AChannels, "achannels", 2, "audio channel count")
	fs.IntVar(&c.VMap, "vmap", 0, "video stream ff-index")
	fs.IntVar(&c.AMap, "amap", 1, "audio stream ff-index")
	fs.StringVar(&c.SubMap, "smap", "", "embedded subtitle stream ff-index (empty = no subtitles)")
	fs.StringVar(&c.SubPath, "subpath", "", "external subtitle file (used as input 1; overrides -smap)")
	fs.StringVar(&c.SubLang, "sublang", "", "subtitle language tag for the HLS rendition")
	fs.StringVar(&c.SubName, "subname", "", "subtitle display name for the HLS rendition")
	fs.StringVar(&rangeHint, "vrange", "", "PQ or HLG when the player decodes an HDR transfer; used for the master's VIDEO-RANGE when init.mp4 has no colr box")
	fs.Float64Var(&idleSec, "idle", 1800, "seconds with no playlist/segment requests after packaging completes before the cast is stopped and swept (0 = serve forever)")
	fs.Parse(argv)

	// own is this instance's claim on the output directory, and every
	// cleanup decision funnels through it: a sweep happens only while the
	// claim holds. Before Acquire the files may belong to a cast another
	// instance is still serving; after a takeover they belong to the new
	// one — in both cases this instance deletes nothing.
	own := NewOwnership(c.OutDir)
	// stopJob is replaced once ffmpeg is running; fail can run before that.
	stopJob := func() {}
	fail := func(msg string) {
		stopJob()
		own.Sweep()
		own.Release()
		Emit(os.Stdout, "error", map[string]any{"msg": msg})
		os.Exit(1)
	}

	if c.Source == "" || c.OutDir == "" || c.FFmpeg == "" || parent == 0 {
		fail("missing required flags: -source, -out, -ffmpeg, -parent")
	}
	if _, err := os.Stat(c.FFmpeg); err != nil {
		fail("ffmpeg not found at " + c.FFmpeg + " — reinstall the plugin through IINA's plugin installer")
	}
	if err := os.MkdirAll(c.OutDir, 0o755); err != nil {
		fail(err.Error())
	}
	if err := own.Acquire(); err != nil {
		fail("stale instance: " + err.Error())
	}
	// We own the directory now; this clears leftovers of a previous
	// instance that died without sweeping (kill -9). Every later sweep
	// goes through teardown.
	own.Sweep()

	var srcSize int64
	if fi, serr := os.Stat(c.Source); serr == nil {
		srcSize = fi.Size()
	}
	port, shutdown, activity, err := StartServer(c.OutDir, c.SubName, c.SubLang, AdvertisedBandwidth(c, srcSize), rangeHint)
	if err != nil {
		fail(err.Error())
	}
	defer shutdown()

	ip, err := LanIP(ipOverride)
	if err != nil {
		fail(err.Error())
	}

	var done <-chan error
	stopJob, done = RunJob(c, os.Stdout)

	// teardown is every non-error way out: stop ffmpeg (blocks until it has
	// exited — a segment it was still flushing would otherwise reappear
	// after the sweep), then sweep, but only while we still own the
	// directory. The remux leaves with the cast, not at the next one — a
	// UHD remux is tens of gigabytes of temp (issue #12). The sync.Once
	// also serializes the watchdog, the ownership poll and the signal
	// handler racing each other onto the same exit.
	var teardownOnce sync.Once
	teardown := func() {
		teardownOnce.Do(func() {
			stopJob()
			own.Sweep()
			own.Release()
		})
	}

	// quit carries every asynchronous reason to end the cast into the serve
	// loops below, so teardown and the exit always happen on the main
	// goroutine — never concurrently with the ffmpeg-done branch, which
	// would otherwise read the teardown's own ffmpeg kill as a job failure.
	quit := make(chan string, 1)
	requestQuit := func(reason string) {
		select {
		case quit <- reason:
		default:
		}
	}

	// IINA gone (quit, crash, kill -9): the cast ends with it.
	WatchParent(parent, func() { requestQuit("parent-gone") })

	// Ownership is lost without a signal when a newer cast (or `stop`)
	// takes the directory: TakeOver removes the pidfile before it signals
	// anything. Poll for that — the only stop channel that exists on every
	// platform — and then leave the files alone: they are the new cast's.
	// This also covers the watchdog's blind spot: IINA may be gone for up
	// to a poll interval while a new cast is already taking over.
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			if !own.Mine() {
				requestQuit("superseded")
				return
			}
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	// The TV can start pulling as soon as the advertised playlist exists.
	// With subs that's ffmpeg's master playlist — but the variant playlists
	// it points at (media + subtitle rendition) must exist too before the
	// URL is usable.
	playlistName := c.PlaylistName()
	readyURL := fmt.Sprintf("http://%s:%d/%s", ip, port, playlistName)
	playlistReady := func() bool {
		if _, err := os.Stat(filepath.Join(c.OutDir, playlistName)); err != nil {
			return false
		}
		if c.HasSub() {
			if _, err := os.Stat(filepath.Join(c.OutDir, "index.m3u8")); err != nil {
				return false
			}
			// master.m3u8's subtitle rendition URI= points at this file; the
			// TV will 404 on it if the master is advertised before it exists.
			if _, err := os.Stat(filepath.Join(c.OutDir, "index_vtt.m3u8")); err != nil {
				return false
			}
		}
		return true
	}
	readyDeadline := time.Now().Add(120 * time.Second)
	readyTick := time.NewTicker(200 * time.Millisecond)
	defer readyTick.Stop()
	ready := false
	idleTimeout := time.Duration(idleSec * float64(time.Second))
	for {
		select {
		case <-readyTick.C:
			if !ready {
				if playlistReady() {
					Emit(os.Stdout, "ready", map[string]any{"url": readyURL})
					ready = true
					readyTick.Stop()
				} else if time.Now().After(readyDeadline) {
					fail("packaging produced no playlist within 120s")
				}
			}
		case err := <-done:
			if err != nil {
				fail(err.Error())
			}
			if !ready {
				// Job finished before the poll noticed the playlist (tiny file).
				if playlistReady() {
					Emit(os.Stdout, "ready", map[string]any{"url": readyURL})
					ready = true
					readyTick.Stop()
				} else {
					fail("ffmpeg finished but produced no playlist")
				}
			}
			// Packaging is done; what remains is serving the finished VOD
			// until someone stops it — or until nobody has pulled a byte for
			// the idle timeout. An unwatched finished cast is tens of
			// gigabytes of temp nobody will ever play, so it gets the same
			// clear result and cleanup as a stopped one.
			serveUntilStopped(idleTimeout, activity, sigs, quit, teardown)
			return
		case <-sigs:
			teardown()
			Emit(os.Stdout, "stopped", nil)
			return
		case reason := <-quit:
			teardown()
			Emit(os.Stdout, "stopped", map[string]any{"reason": reason})
			return
		}
	}
}

// serveUntilStopped blocks once packaging has completed: the cast ends on a
// stop signal, an asynchronous quit (parent gone, superseded), or the idle
// timeout when no playlist or segment has been requested since. teardown
// runs first in every case, so the process only ever exits after ffmpeg is
// gone and the sweep (if still owned) is done.
func serveUntilStopped(idleTimeout time.Duration, activity *Activity, sigs <-chan os.Signal, quit <-chan string, teardown func()) {
	var idleC <-chan time.Time
	if idleTimeout > 0 {
		t := time.NewTicker(idleCheckInterval(idleTimeout))
		defer t.Stop()
		idleC = t.C
	}
	packagedAt := time.Now()
	for {
		select {
		case <-sigs:
			teardown()
			Emit(os.Stdout, "stopped", nil)
			return
		case reason := <-quit:
			teardown()
			Emit(os.Stdout, "stopped", map[string]any{"reason": reason})
			return
		case <-idleC:
			last := activity.Last()
			if last.Before(packagedAt) {
				last = packagedAt
			}
			if time.Since(last) >= idleTimeout {
				teardown()
				Emit(os.Stdout, "stopped", map[string]any{"reason": "idle"})
				return
			}
		}
	}
}

// idleCheckInterval paces the idle check to the timeout: often enough that
// the timeout is honoured closely, never faster than 100ms.
func idleCheckInterval(timeout time.Duration) time.Duration {
	d := timeout / 4
	if d < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	if d > 5*time.Second {
		return 5 * time.Second
	}
	return d
}
