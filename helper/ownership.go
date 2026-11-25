package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Ownership is one serve instance's claim on the output directory, and the
// single answer to "may I delete these files?". Packaging and cleanup both
// revolve around it: only the instance whose pid the directory's pidfile
// names may sweep. Losing the claim — a newer cast or the `stop` subcommand
// removes/replaces the pidfile before signaling (TakeOver) — means the
// files there belong to someone else now, and a superseded instance must
// not delete any of them on its way out.
type Ownership struct {
	dir     string
	pidfile string
	mu      sync.Mutex
	held    bool
}

func NewOwnership(dir string) *Ownership {
	return &Ownership{dir: dir, pidfile: filepath.Join(dir, "helper.pid")}
}

// Acquire takes the directory from any previous instance and plants our
// pidfile. Until it succeeds, Sweep and Release are no-ops: the files in
// the directory may belong to a cast another instance is still serving.
func (o *Ownership) Acquire() error {
	if err := TakeOver(o.pidfile); err != nil {
		return err
	}
	if err := WritePidfile(o.pidfile); err != nil {
		return err
	}
	o.mu.Lock()
	o.held = true
	o.mu.Unlock()
	return nil
}

// Mine reports whether the directory is still ours: acquired, and the
// pidfile still names this process. A missing or foreign pidfile means a
// newer instance took over.
func (o *Ownership) Mine() bool {
	o.mu.Lock()
	held := o.held
	o.mu.Unlock()
	if !held {
		return false
	}
	b, err := os.ReadFile(o.pidfile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && pid == os.Getpid()
}

// Sweep removes this cast's artifacts, but only while the directory is
// still ours — a superseded instance must not delete the new cast's
// freshly written segments.
func (o *Ownership) Sweep() {
	if !o.Mine() {
		return
	}
	cleanOutDir(o.dir)
}

// Release drops the pidfile if it is still ours. After a takeover it isn't,
// and removing the new owner's pidfile would orphan its cast.
func (o *Ownership) Release() {
	if o.Mine() {
		os.Remove(o.pidfile)
	}
	o.mu.Lock()
	o.held = false
	o.mu.Unlock()
}
