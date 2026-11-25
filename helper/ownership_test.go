package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Sweep before Acquire must be a no-op: the files in the directory may
// belong to a cast another instance is still serving.
func TestOwnershipSweepBeforeAcquireIsNoop(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "index.m3u8")
	os.WriteFile(f, []byte("#EXTM3U\n"), 0o644)
	own := NewOwnership(dir)
	if own.Mine() {
		t.Fatal("unacquired ownership must not be mine")
	}
	own.Sweep()
	if _, err := os.Stat(f); err != nil {
		t.Fatal("sweep before acquire deleted a file")
	}
	own.Release() // must not panic or remove anything
}

// The owner sweeps its own cast's artifacts, and Release drops its pidfile.
func TestOwnershipSweepWhileMine(t *testing.T) {
	dir := t.TempDir()
	own := NewOwnership(dir)
	if err := own.Acquire(); err != nil {
		t.Fatal(err)
	}
	if !own.Mine() {
		t.Fatal("freshly acquired ownership must be mine")
	}
	f := filepath.Join(dir, "seg_0000.m4s")
	os.WriteFile(f, []byte("seg"), 0o644)
	own.Sweep()
	if _, err := os.Stat(f); err == nil {
		t.Fatal("owner's sweep left a segment behind")
	}
	own.Release()
	if _, err := os.Stat(own.pidfile); !os.IsNotExist(err) {
		t.Fatal("Release must remove our own pidfile")
	}
}

// The invariant the whole design rests on: once the pidfile names someone
// else, Sweep deletes nothing and Release leaves the foreign pidfile alone.
func TestOwnershipLostDisarmsSweepAndRelease(t *testing.T) {
	dir := t.TempDir()
	own := NewOwnership(dir)
	if err := own.Acquire(); err != nil {
		t.Fatal(err)
	}
	// A newer instance takes over: its pidfile replaces ours, and its
	// freshly written segments land in the directory.
	if err := os.WriteFile(own.pidfile, []byte("123456"), 0o644); err != nil {
		t.Fatal(err)
	}
	seg := filepath.Join(dir, "seg_0042.m4s")
	os.WriteFile(seg, []byte("seg"), 0o644)

	if own.Mine() {
		t.Fatal("foreign pidfile must end our ownership")
	}
	own.Sweep()
	if _, err := os.Stat(seg); err != nil {
		t.Fatal("superseded instance swept the new cast's segment")
	}
	own.Release()
	b, err := os.ReadFile(own.pidfile)
	if err != nil || string(b) != "123456" {
		t.Fatalf("Release disturbed the new owner's pidfile: %q, %v", b, err)
	}
}

// A missing pidfile (TakeOver removes it before signaling) is just as much
// "not ours anymore" as a foreign one.
func TestOwnershipMissingPidfileIsNotMine(t *testing.T) {
	dir := t.TempDir()
	own := NewOwnership(dir)
	if err := own.Acquire(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(own.pidfile); err != nil {
		t.Fatal(err)
	}
	if own.Mine() {
		t.Fatal("removed pidfile must end our ownership")
	}
	seg := filepath.Join(dir, "seg_0001.m4s")
	os.WriteFile(seg, []byte("seg"), 0o644)
	own.Sweep()
	if _, err := os.Stat(seg); err != nil {
		t.Fatal("sweep ran after ownership was lost")
	}
}

// Acquire must clear out a live predecessor: after it returns, the
// predecessor is gone and the pidfile names us.
func TestOwnershipAcquireTakesOver(t *testing.T) {
	dir := t.TempDir()
	prev := startStubProcess(t, "sleep")
	pidfile := filepath.Join(dir, "helper.pid")
	os.WriteFile(pidfile, []byte(strconv.Itoa(prev.Process.Pid)), 0o644)

	own := NewOwnership(dir)
	if err := own.Acquire(); err != nil {
		t.Fatal(err)
	}
	if !own.Mine() {
		t.Fatal("acquired ownership must be mine")
	}
	if pid, err := readPidfile(pidfile); err != nil || pid != os.Getpid() {
		t.Fatalf("pidfile should name us: pid=%d err=%v", pid, err)
	}
	done := make(chan error, 1)
	go func() { done <- prev.Wait() }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("predecessor survived Acquire")
	}
}
