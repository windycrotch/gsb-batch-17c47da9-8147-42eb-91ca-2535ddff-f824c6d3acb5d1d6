/*

Per-output locking so that two ffcvt processes batch-transcoding the
same directory can never write the same output file at once.

A lock is a sidecar file ("<output>.ffcvt-lock") holding the owning
process id and start time. It is created with O_CREATE|O_EXCL, which is
atomic on every supported platform, so exactly one process wins. A lock
whose owning process is no longer alive (or whose age exceeds a sanity
horizon, in case pid liveness cannot be established) is treated as
stale and reclaimed. The loser waits briefly for the owner to finish and
either proceeds once the output appears, or skips the file safely.

*/

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// lockWait controls how long a losing process waits for the owner to
// finish before skipping. Overridable in tests. In production it is long
// enough to ride out a normal encode; the wait loop nevertheless returns
// the instant the owner publishes a result, so it never actually sleeps
// for the full duration once the output is available.
var lockWait = 30 * time.Second

const lockPollInterval = 50 * time.Millisecond

// lockStaleHorizon is the upper bound on a lock's age before it is
// reclaimed even if the recorded pid still appears to exist.
const lockStaleHorizon = 24 * time.Hour

type lockData struct {
	pid       int
	startedAt time.Time
}

func lockPathFor(outputName string) string {
	return outputName + ".ffcvt-lock"
}

func (l lockData) serialize() string {
	return fmt.Sprintf("pid=%d\nstarted_at=%s\n", l.pid, startedAtRFC3339(l.startedAt))
}

func startedAtRFC3339(t time.Time) string { return t.Format(time.RFC3339) }

func parseLockData(p string) (lockData, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return lockData{}, err
	}
	var d lockData
	for _, line := range strings.Split(string(raw), "\n") {
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "pid":
			d.pid, _ = strconv.Atoi(strings.TrimSpace(kv[1]))
		case "started_at":
			d.startedAt, _ = time.Parse(time.RFC3339, strings.TrimSpace(kv[1]))
		}
	}
	if d.pid == 0 {
		return d, fmt.Errorf("invalid lock file %s", p)
	}
	return d, nil
}

// isStale decides whether an existing lock may be reclaimed.
func isStale(p string) bool {
	d, err := parseLockData(p)
	if err != nil {
		return true // unreadable lock: never block the batch forever
	}
	if !processAlive(d.pid) {
		return true
	}
	if !d.startedAt.IsZero() && time.Since(d.startedAt) > lockStaleHorizon {
		return true
	}
	return false
}

// tryCreateLock atomically claims the lock sidecar.
func tryCreateLock(p string) bool {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return false
	}
	defer f.Close()
	_, _ = f.WriteString(lockData{pid: os.Getpid(), startedAt: time.Now()}.serialize())
	return true
}

// acquireOutputLock claims exclusive ownership of an output. The returned
// unlock function is non-nil on success; lockedByOther is true when
// another live process owns the output and the caller should skip it.
func acquireOutputLock(outputName string) (unlock func(), lockedByOther bool) {
	lockP := lockPathFor(outputName)
	if err := os.MkdirAll(filepath.Dir(lockP), os.ModePerm); err != nil {
		// If we cannot even create the directory, let transcodeFile
		// surface the real error; proceed without a lock.
		return func() {}, false
	}

	if tryCreateLock(lockP) {
		return func() { _ = os.Remove(lockP) }, false
	}

	// Someone else holds it. Reclaim stale locks, otherwise wait for
	// the owner to finish within lockWait. The loop returns promptly as
	// soon as the owner publishes a finished output or a durable journal
	// result, rather than sleeping out the whole deadline.
	deadline := time.Now().Add(lockWait)
	for {
		if isStale(lockP) {
			_ = os.Remove(lockP)
			if tryCreateLock(lockP) {
				return func() { _ = os.Remove(lockP) }, false
			}
		}
		// The owner finished successfully: its verified output is in
		// place. Skip safely instead of overwriting it.
		if fileExist(outputName) {
			return nil, true
		}
		// The owner already committed a durable result for this output
		// (success or failure). Do not touch its output.
		if ownerRecordedResult(outputName) {
			return nil, true
		}
		if time.Now().After(deadline) {
			// Timed out while the owner is still alive: skip safely
			// rather than risk writing over an in-progress output.
			return nil, true
		}
		time.Sleep(lockPollInterval)
	}
}

// ownerRecordedResult reports whether the output's input already has a
// journal entry from another process. It re-reads the on-disk state so
// entries committed by a concurrent process after this one started are
// observed. Best-effort: any unreadable state yields false and the
// caller keeps waiting.
func ownerRecordedResult(outputName string) bool {
	data, err := os.ReadFile(journalPath())
	if err != nil {
		return false
	}
	var fresh Journal
	if json.Unmarshal(data, &fresh) != nil {
		return false
	}
	for _, st := range fresh.Files {
		if st != nil && st.Output == outputName {
			return true
		}
	}
	return false
}
