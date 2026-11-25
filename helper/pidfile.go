package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// WritePidfile plants our pid atomically (temp file + rename): the
// ownership poll reads this file constantly and must never observe a
// half-written pid.
func WritePidfile(path string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return err
	}
	// Windows cannot rename over an existing file, and a concurrent reader
	// momentarily holding the old pidfile open makes even the remove+rename
	// pair fail transiently — retry a few times before giving up.
	var err error
	for range 25 {
		os.Remove(path)
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	os.Remove(tmp)
	return err
}

// readPidfile returns the pid a pidfile names.
func readPidfile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("bad pidfile %q", path)
	}
	return pid, nil
}

// TakeOver ends the previous instance's claim on the output directory. The
// pidfile is removed FIRST: that, not the signal, is what transfers
// ownership. The old instance's teardown checks ownership before sweeping,
// so once the pidfile is gone it cannot delete anything — including files
// the next instance is about to write. Only then is the old process asked
// to exit (on Windows there is no signal to send; its ownership poll
// notices the missing pidfile instead), and if it ignores the request it is
// force-killed so the takeover always completes.
func TakeOver(path string) error {
	pid, err := readPidfile(path)
	if err != nil {
		// A missing pidfile means nothing to take over from. An unreadable
		// one is still removed below: the goal is that no stale pidfile
		// survives, and by the end of this function none does.
		if !os.IsNotExist(err) {
			pid = 0
		} else {
			return nil
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if pid <= 0 || !processAlive(pid) {
		return nil
	}
	terminatePid(pid)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		forceKillPid(pid)
		deadline = time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && processAlive(pid) {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}
