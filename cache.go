package tilepix

import (
	"os"
	"path/filepath"
	"sync"
)

/*
  _____ _ _
 |_   _(_) |___ ___ ___ _  _ _ _  ___
   | | | | / -_) -_) _ \ || | ' \/ -_)
   |_| |_|_\___\___\___/\_,_|_||_\___|
*/

// inFlight represents a single load of a map path which may be shared by
// several concurrent callers.  Every caller waits on the same result, so it is
// impossible for one caller to observe a map which another caller is still
// assembling, or to see layers and objects belonging to a different map.
type inFlight struct {
	done chan struct{}
	m    *Map
	err  error
}

// mapCache deduplicates concurrent loads of the same path (single-flight).
//
// A load is held in the cache only while it is in progress: once it completes
// the entry is removed, whether it succeeded or failed.  This means that
//
//   - concurrent calls for one path share a single load and all receive the
//     same complete map (or the same error),
//   - a later, sequential call for the same path performs a fresh load which
//     builds its own batches and sprites, so state from the previous map can
//     never leak into the new one,
//   - a failed load is never recorded, so retrying the same path re-attempts
//     the load instead of being stuck in a failed state, and
//   - a map which was returned successfully earlier is untouched by any later
//     failed load and can keep being drawn and queried.
type mapCache struct {
	mu      sync.Mutex
	flights map[string]*inFlight
}

var maps = &mapCache{
	flights: make(map[string]*inFlight),
}

// loadFile loads the map at filePath, deduplicating concurrent calls for the
// same path.  The first caller runs load while any concurrent callers wait for
// its result; all callers receive exactly the same map instance (or the same
// error).  Sequential calls are never served from a stored result and always
// trigger a fresh, isolated load.
func (c *mapCache) loadFile(filePath string, load func() (*Map, error)) (*Map, error) {
	key := cacheKey(filePath)

	c.mu.Lock()
	if flight, ok := c.flights[key]; ok {
		// Another goroutine is already loading this path - wait for its
		// result rather than starting a competing load.
		c.mu.Unlock()
		<-flight.done
		return flight.m, flight.err
	}

	flight := &inFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()

	// Only one goroutine ever reaches this point for a given path at a time.
	m, err := load()

	c.mu.Lock()
	// Always remove the in-flight entry: success and failure are both
	// terminal for this load, and neither result is retained.
	delete(c.flights, key)
	flight.m, flight.err = m, err
	close(flight.done)
	c.mu.Unlock()

	return m, err
}

// cacheKey normalises a file path so equivalent paths share an in-flight load.
// If the path cannot be made absolute (for example a relative path on a
// deleted working directory) the original path is used unchanged.
func cacheKey(filePath string) string {
	abs, err := filepath.Abs(filePath)
	if err != nil {
		return filepath.Clean(filePath)
	}
	return filepath.Clean(abs)
}

// loadMapFromFile is the single, uncached load of a map file.  It is kept out
// of ReadFile so the de-duplication logic and the file handling stay separate.
func loadMapFromFile(filePath string) (*Map, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return Read(f, filepath.Dir(filePath), nil)
}
