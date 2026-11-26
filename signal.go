/*

Signal handling for graceful interruption. When the user (or the
operating system) asks ffcvt to stop, the current file finishes or is
aborted, completed results are kept, the in-flight file is cleaned up as
a failure (never left behind as a bogus success), and the next run
resumes from the remaining files via the journal.

*/

package main

import (
	"context"
	"os/signal"
)

// contextCanceller wraps a signal-bound context so the batch driver can
// check for interruption without importing context everywhere.
type contextCanceller struct {
	c      context.Context
	cancel context.CancelFunc
}

func (c *contextCanceller) done() bool {
	if c == nil || c.c == nil {
		return false
	}
	select {
	case <-c.c.Done():
		return true
	default:
		return false
	}
}

// ctx returns the underlying context, or a background one when signals
// were not wired up (e.g. unit tests).
func (c *contextCanceller) ctx() context.Context {
	if c == nil || c.c == nil {
		return context.Background()
	}
	return c.c
}

// setupSignals wires termination signals into the global batchCancel.
func setupSignals() {
	ctx, cancel := signal.NotifyContext(context.Background(), terminateSignals()...)
	batchCancel = &contextCanceller{c: ctx, cancel: cancel}
}
