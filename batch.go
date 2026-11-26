/*

Batch transcoding orchestration: per-file outcome tracking, a durable
journal for resumable runs, atomic output placement, per-output locking
for concurrent processes, and a trustworthy end-of-run summary.

Each input file ends in exactly one of three outcomes:
  - success: the output file is complete and readable at its final name
  - failure: no file is left at the output name that could be mistaken
    for a finished product, and the failure is recorded in the journal
  - skip:    an earlier successful run (or another live process) owns it

*/

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// FileResult is the final outcome of a single input file.
type FileResult string

const (
	ResultSuccess FileResult = "success"
	ResultFailure FileResult = "failure"
	ResultSkip    FileResult = "skip"
)

// Exit codes reported by the top-level driver.
const (
	exitOK          = 0
	exitFailure     = 1
	exitInterrupted = 2
)

// FileState is one journal entry, keyed by the absolute input path.
type FileState struct {
	Result     FileResult `json:"result"`
	Output     string     `json:"output"`
	InputSize  int64      `json:"input_size"`
	OutputSize int64      `json:"output_size"`
	Error      string     `json:"error,omitempty"`
	Attempts   int        `json:"attempts"`
	UpdatedAt  string     `json:"updated_at"`
}

// Journal is the on-disk, resumable state of a batch run.
type Journal struct {
	Dir   string                `json:"-"`
	Files map[string]*FileState `json:"files"`
}

// BatchStats accumulates the trustworthy end-of-run totals. Bytes are
// raw byte counts (not the rounded KB/MB shown in the legacy output).
type BatchStats struct {
	Success   int
	Failure   int
	Skip      int
	Locked    int   // subset of Skip: busy with another live process
	OrgBytes  int64 // successful inputs only
	NewBytes  int64 // successful outputs only
	SkipBytes int64 // bytes of skipped inputs
	FailBytes int64 // bytes of failed inputs (kept out of savings math)
}

////////////////////////////////////////////////////////////////////////////
// Global variables definitions

// batchCancel is wired to the process signals (see signal_*.go).
var batchCancel *contextCanceller

////////////////////////////////////////////////////////////////////////////
// Function definitions

// outputRoot returns the directory tree under which transcoded outputs
// are written, so the journal can live beside them. In work-directory
// (-w) mode main() already re-roots Opts.WDirectory at the source
// directory's basename, so it is the output root directly.
func outputRoot() string {
	if Opts.WDirectory != "" {
		return Opts.WDirectory
	}
	return Opts.Directory
}

// journalPath returns the location of the durable state file. It lives
// at the root of where transcoded outputs are placed so that reruns of
// the same directory find it again.
func journalPath() string {
	return filepath.Join(outputRoot(), ".ffcvt-state.json")
}

// loadJournal reads the durable state. A missing or corrupt journal is
// treated as an empty one: a fresh run simply redoes the work.
func loadJournal() *Journal {
	j := &Journal{Dir: Opts.Directory, Files: map[string]*FileState{}}
	data, err := os.ReadFile(journalPath())
	if err != nil {
		return j
	}
	if err := json.Unmarshal(data, j); err != nil {
		log.Printf("%s: State file unreadable, starting fresh - %s",
			progname, err.Error())
		return &Journal{Dir: Opts.Directory, Files: map[string]*FileState{}}
	}
	if j.Files == nil {
		j.Files = map[string]*FileState{}
	}
	return j
}

// save writes the journal atomically (unique temp file + rename) so a
// crash mid-write can never leave a truncated state file behind. It
// takes a short-lived journal lock and re-reads the on-disk state first,
// merging any entries another concurrent process committed since this
// process loaded its copy.
func (j *Journal) save() {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		log.Printf("%s: State marshal error - %s", progname, err.Error())
		return
	}
	p := journalPath()
	if err := os.MkdirAll(filepath.Dir(p), os.ModePerm); err != nil {
		log.Printf("%s: State dir error - %s", progname, err.Error())
		return
	}

	unlock, busy := acquireOutputLock(p)
	if busy {
		// Another process is committing right now; retry briefly. The
		// per-output content locks still guarantee no output corruption;
		// this only protects the shared journal's completeness.
		for attempt := 0; attempt < 50 && busy; attempt++ {
			time.Sleep(20 * time.Millisecond)
			unlock, busy = acquireOutputLock(p)
		}
		if busy {
			log.Printf("%s: State file busy, journal update deferred", progname)
			return
		}
	}
	defer unlock()

	// Merge entries committed by a concurrent process after our load.
	if onDisk, err := os.ReadFile(p); err == nil {
		var other Journal
		if json.Unmarshal(onDisk, &other) == nil {
			for k, v := range other.Files {
				if _, have := j.Files[k]; !have {
					j.Files[k] = v
				}
			}
			data, _ = json.MarshalIndent(j, "", "  ")
		}
	}

	tmp := fmt.Sprintf("%s.tmp-%d-%d", p, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("%s: State write error - %s", progname, err.Error())
		return
	}
	if err := atomicRename(tmp, p); err != nil {
		log.Printf("%s: State rename error - %s", progname, err.Error())
		_ = os.Remove(tmp)
	}
}

// lastBatchStats exposes the most recent run's totals to tests. In
// production it is simply overwritten each runBatch invocation.
var lastBatchStats BatchStats

// rerunJournal is the journal loaded once per batch run, shared with
// the queueing logic so -nc can recognize already-finished inputs.
var rerunJournal *Journal

// isAlreadyDone reports whether an input was completed successfully in
// an earlier run and its output is still present. A failed or
// interrupted input is not done and will be retried.
func isAlreadyDone(inputName string) bool {
	if rerunJournal == nil {
		return false
	}
	key, err := filepath.Abs(inputName)
	if err != nil {
		return false
	}
	st := rerunJournal.Files[key]
	if st == nil || st.Result != ResultSuccess {
		return false
	}
	return fileExist(st.Output)
}

// freshJournalHasSuccess re-reads the on-disk journal and reports whether
// it records a successful result for the given input/output. It lets a
// process observe results committed by a concurrent process.
func freshJournalHasSuccess(key, outputName string) bool {
	data, err := os.ReadFile(journalPath())
	if err != nil {
		return false
	}
	var fresh Journal
	if json.Unmarshal(data, &fresh) != nil {
		return false
	}
	if st := fresh.Files[key]; st != nil && st.Result == ResultSuccess {
		return true
	}
	for _, st := range fresh.Files {
		if st != nil && st.Result == ResultSuccess && st.Output == outputName {
			return true
		}
	}
	return false
}

// runBatch drives the whole-directory transcode and returns the process
// exit code. It never calls os.Exit itself so that deferred cleanup and
// the summary always run, even on interruption and -maxc.
func runBatch(startTime time.Time) int {
	videosTotal := len(vidCol.videos)
	if videosTotal > 0 {
		totalSize := vidCol.sum
		for i := range vidCol.videos {
			vidCol.videos[i].pct =
				int((vidCol.videos[i].sum*1000/totalSize + 5) / 10)
		}
	}

	journal := loadJournal()
	rerunJournal = journal
	stats := BatchStats{}
	// Inputs filtered out during queueing (-nc: already finished) are
	// still part of this run's outcome and must appear in the summary.
	for _, sv := range vidCol.skipped {
		stats.Skip++
		stats.SkipBytes += sv.size
	}
	interrupted := false
	processed := 0

	for i, v := range vidCol.videos {
		inputName := v.name
		videoNdx := i + 1
		outputName := getOutputName(inputName)

		if batchCancel != nil && batchCancel.done() {
			// Interruption: untouched files must not look finished and
			// must simply be retried on the next run.
			interrupted = true
			break
		}

		// Already done in a previous run: skip rather than re-encode.
		// The journal is authoritative; the on-disk output is double
		// checked so that a manually deleted output gets redone.
		key, _ := filepath.Abs(inputName)
		if st := journal.Files[key]; st != nil && st.Result == ResultSuccess &&
			fileExist(st.Output) {
			fmt.Printf("\n== Skipping [%d/%d] (already done): '%s'\n",
				videoNdx, videosTotal, filepath.Base(inputName))
			stats.Skip++
			stats.SkipBytes += v.size
			continue
		}

		fmt.Printf("\n== Transcoding [%d/%d] (%d%%): '%s'\n   under %s\n",
			videoNdx, videosTotal, v.pct,
			filepath.Base(inputName), filepath.Dir(inputName))

		// Take an exclusive lock on the output so that a second process
		// running on the same directory cannot write the same file.
		unlock, lockedByOther := acquireOutputLock(outputName)
		if lockedByOther {
			// The other process is alive and owns this output: skip it
			// safely instead of clobbering its work.
			fmt.Printf("== Skipping [%d/%d]: output is locked by another process: '%s'\n",
				videoNdx, videosTotal, filepath.Base(inputName))
			stats.Skip++
			stats.Locked++
			stats.SkipBytes += v.size
			continue
		}

		// Re-check after acquiring the lock. A concurrent process may
		// have finished this output (and released the lock) between our
		// earlier checks; trust the on-disk output and a freshly read
		// journal, not this process's possibly stale in-memory copy.
		if fileExist(outputName) || freshJournalHasSuccess(key, outputName) {
			unlock()
			stats.Skip++
			stats.SkipBytes += v.size
			continue
		}

		res, outSize, errMsg := transcodeFile(batchCancel.ctx(), inputName)
		if res == ResultSkip {
			// Dry run (-n) or an -nc output that appeared between checks:
			// nothing was encoded and nothing may be counted as success.
			unlock()
			stats.Skip++
			stats.SkipBytes += v.size
			continue
		}
		processed++

		state := &FileState{
			Output:    outputName,
			InputSize: v.size,
			UpdatedAt: time.Now().Format(time.RFC3339),
		}
		if prev := journal.Files[key]; prev != nil {
			state.Attempts = prev.Attempts
		}
		state.Attempts++

		switch res {
		case ResultSuccess:
			state.Result = ResultSuccess
			state.OutputSize = outSize
			journal.Files[key] = state
			// Journal under the lock so a concurrent process observes
			// either "locked" or a complete success, never a half state.
			journal.save()
			unlock()

			stats.Success++
			stats.OrgBytes += v.size
			stats.NewBytes += outSize
		case ResultFailure:
			state.Result = ResultFailure
			state.Error = errMsg
			journal.Files[key] = state
			journal.save()
			unlock()

			stats.Failure++
			stats.FailBytes += v.size
			fmt.Printf("== Failed (continuing with the rest): '%s': %s\n",
				filepath.Base(inputName), errMsg)
		}

		fmt.Printf("Time taken so far %s\n", time.Since(startTime))

		// -maxc bounds the number of conversions attempted this run;
		// unprocessed files are neither successes nor failures, and the
		// summary is still printed below.
		if Opts.MaxC != 0 && processed >= Opts.MaxC {
			debug("Max conversion count reached.", 1)
			break
		}
		debug("Taking a breath for: "+Opts.BreathTime.String(), 1)
		time.Sleep(Opts.BreathTime)
		if i < len(vidCol.videos)-1 && v.pct > 0 {
			fmt.Printf("Finishing the remaining %d%% in %s\n",
				100-v.pct,
				time.Duration(int64(float32(time.Since(startTime))*
					float32(100-v.pct)/float32(v.pct))))
		}
	}

	printSummary(stats, time.Since(startTime))
	lastBatchStats = stats

	if interrupted {
		return exitInterrupted
	}
	if stats.Failure > 0 {
		return exitFailure
	}
	return exitOK
}

// printSummary reports counts and byte totals that reconcile against
// each other. Failed inputs are deliberately excluded from both the
// numerator and the denominator of the space-saved figure.
func printSummary(stats BatchStats, elapsed time.Duration) {
	total := stats.Success + stats.Failure + stats.Skip
	var savedPct int64
	if stats.OrgBytes > 0 {
		savedPct = (stats.OrgBytes - stats.NewBytes) * 100 / stats.OrgBytes
	}

	fmt.Printf("\nTranscoding completed in %s\n", elapsed)
	fmt.Printf("Summary: %d file(s) total — %d succeeded, %d failed, %d skipped",
		total, stats.Success, stats.Failure, stats.Skip)
	if stats.Locked > 0 {
		fmt.Printf(" (%d locked by another process)", stats.Locked)
	}
	fmt.Println()
	fmt.Printf("Success: %d file(s), %d input bytes -> %d output bytes\n",
		stats.Success, stats.OrgBytes, stats.NewBytes)
	fmt.Printf("Failure: %d file(s), %d input bytes (excluded from space savings)\n",
		stats.Failure, stats.FailBytes)
	fmt.Printf("Skipped: %d file(s), %d input bytes\n",
		stats.Skip, stats.SkipBytes)
	fmt.Printf("Org Size: %d MB\n", stats.OrgBytes/1024/1024)
	fmt.Printf("New Size: %d MB\n", stats.NewBytes/1024/1024)
	fmt.Printf("Saved:    %d%%\n", savedPct)

	// Machine-parseable line for automated tests/tooling.
	fmt.Printf("FFCVT_SUMMARY total=%d success=%d failure=%d skip=%d "+
		"org_bytes=%d new_bytes=%d fail_bytes=%d skip_bytes=%d saved_pct=%d\n",
		total, stats.Success, stats.Failure, stats.Skip,
		stats.OrgBytes, stats.NewBytes, stats.FailBytes,
		stats.SkipBytes, savedPct)
}
