package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The integration tests need a fake ffmpeg (and the occasional stand-in
// parent process) that a real helper process can exec. That used to be
// #!/bin/sh scripts, which no Windows machine can run — so the test binary
// doubles as the stub: spawned with AIRPLAY_STUB_MODE set, TestMain diverts
// it into the requested role before any test (or flag parsing) runs. The
// acceptance shape is unchanged: real helper processes, real stdout events,
// real output directories.
func TestMain(m *testing.M) {
	if mode := os.Getenv("AIRPLAY_STUB_MODE"); mode != "" {
		os.Exit(runStub(mode))
	}
	os.Exit(m.Run())
}

// runStub acts out a process role driven entirely by environment variables,
// because RunJob (and the helper under test) execs a bare binary path with
// no control over its arguments or environment.
//
//	AIRPLAY_STUB_MODE    which role to play (see below)
//	AIRPLAY_STUB_OUT     output directory for the cast-file-writing roles
//	AIRPLAY_STUB_MASTER  master.m3u8 content for the sub-cast role
func runStub(mode string) int {
	// Die with our parent: a stub outliving the helper (or test) that
	// spawned it would linger for the whole sleep below.
	go func() {
		ppid := os.Getppid()
		for {
			time.Sleep(500 * time.Millisecond)
			if !processAlive(ppid) {
				os.Exit(0)
			}
		}
	}()
	out := os.Getenv("AIRPLAY_STUB_OUT")
	writeCast := func() {
		os.WriteFile(filepath.Join(out, "index.m3u8"), []byte("#EXTM3U\n"), 0o644)
		os.WriteFile(filepath.Join(out, "init.mp4"), []byte("init"), 0o644)
		os.WriteFile(filepath.Join(out, "seg_0000.m4s"), []byte("seg"), 0o644)
	}
	switch mode {
	case "sleep":
		// Stand-in for IINA (the watched parent) or a previous instance:
		// just stay alive until killed.
		blockForever()
	case "idle-cast":
		// ffmpeg mid-packaging: lays down the cast files, reports progress,
		// then runs like a long transcode. Exits 0 on SIGTERM, as real
		// ffmpeg does.
		writeCast()
		fmt.Println("out_time_us=1000000")
		exitOnTerm()
		blockForever()
	case "ignore-term-cast":
		// ffmpeg that ignores SIGTERM: exercises the stop() escalation to a
		// force kill after the grace period.
		writeCast()
		fmt.Println("out_time_us=1000000")
		signal.Ignore(syscall.SIGTERM)
		blockForever()
	case "finish-cast":
		// A tiny file: packaging completes immediately.
		writeCast()
		fmt.Println("out_time_us=1000000")
		fmt.Println("progress=end")
		return 0
	case "fail-cast":
		// Fails part-way, leaving partial output behind.
		writeCast()
		fmt.Fprintln(os.Stderr, "boom")
		return 1
	case "fail-early":
		// Fails before the playlist exists.
		fmt.Fprintln(os.Stderr, "boom")
		return 1
	case "sub-cast":
		// One variant plus a subtitle rendition, as hlsenc writes them.
		os.WriteFile(filepath.Join(out, "index.m3u8"), []byte("#EXTM3U\n"), 0o644)
		os.WriteFile(filepath.Join(out, "index_vtt.m3u8"), []byte("#EXTM3U\n"), 0o644)
		os.WriteFile(filepath.Join(out, "master.m3u8"), []byte(os.Getenv("AIRPLAY_STUB_MASTER")), 0o644)
		fmt.Println("out_time_us=1000000")
		exitOnTerm()
		blockForever()
	case "progress":
		fmt.Println("out_time_us=2700000000")
		fmt.Println("progress=continue")
		fmt.Println("out_time_us=5400000000")
		fmt.Println("progress=end")
		return 0
	case "fail":
		fmt.Fprintln(os.Stderr, "Unknown encoder 'nope'")
		return 1
	}
	return 0
}

// blockForever parks the stub until it is signaled or killed. A bare
// select{} would trip the runtime's all-goroutines-asleep deadlock check,
// so block on a timer instead.
func blockForever() {
	for {
		time.Sleep(time.Hour)
	}
}

// exitOnTerm makes the stub die 0 on SIGTERM/SIGINT, the way ffmpeg does.
// On Windows no signal ever arrives; the stub is taskkilled instead.
func exitOnTerm() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-ch
		os.Exit(0)
	}()
}

// useStub points the test binary at a stub role for any child process the
// test (or the helper under test) spawns: exec.Command inherits the test
// process environment. out is the directory the cast-writing roles fill.
func useStub(t *testing.T, mode, out string) {
	t.Helper()
	t.Setenv("AIRPLAY_STUB_MODE", mode)
	t.Setenv("AIRPLAY_STUB_OUT", out)
}

// stubPath is the executable a test hands to the helper as "ffmpeg" (or
// execs directly as a stand-in process): the test binary itself, diverted
// by AIRPLAY_STUB_MODE.
func stubPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// startStubProcess launches a stand-alone stub process (a stand-in parent,
// a foreign pidfile owner) and kills it when the test ends.
func startStubProcess(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(stubPath(t))
	cmd.Env = append(os.Environ(), "AIRPLAY_STUB_MODE="+mode)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}
