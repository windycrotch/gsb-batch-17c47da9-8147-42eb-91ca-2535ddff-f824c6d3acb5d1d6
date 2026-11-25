package hls

import (
	"path/filepath"
	"sync"
)

// dirLockRegistry serializes transcoding tasks that target the same output
// directory. A task holds the lock for its target directory for its whole
// lifetime, so a second concurrent submission to the same directory is
// rejected with ErrTargetBusy instead of interleaving writes into the same
// segment/playlist file names. Different directories still run concurrently.
type dirLockRegistry struct {
	mu    sync.Mutex
	locks map[string]bool
}

func newDirLockRegistry() *dirLockRegistry {
	return &dirLockRegistry{locks: make(map[string]bool)}
}

// tryAcquire takes the lock for target. It returns ErrTargetBusy when another
// task currently owns the lock. The returned release function is idempotent.
func (r *dirLockRegistry) tryAcquire(target string) (func(), error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	// Normalize so e.g. "dir" and "dir/" map to the same key.
	key := filepath.Clean(abs)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.locks[key] {
		return nil, ErrTargetBusy
	}
	r.locks[key] = true

	released := false
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if released {
			return
		}
		released = true
		r.locks[key] = false
	}, nil
}
