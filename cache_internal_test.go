package tilepix

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopxl/pixel"
)

// writeMapFixture creates a self-contained, loadable TMX map in dir.  The map
// has a tile layer plus a flipped object layer so the visible tiles and the
// object co-ordinates can be asserted together.
func writeMapFixture(t *testing.T, dir, name string) {
	t.Helper()

	const tmx = `<?xml version="1.0" encoding="UTF-8"?>
<map version="1.0" orientation="orthogonal" width="8" height="8" tilewidth="16" tileheight="16">
 <tileset firstgid="1" name="atlas" tilewidth="16" tileheight="16" tilecount="1" columns="1">
  <image source="atlas.png" width="16" height="16"/>
 </tileset>
 <layer name="Tile Layer 1" width="8" height="8">
  <data encoding="csv">
1,1,1,1,1,1,1,1,
1,1,1,1,1,1,1,1,
1,1,1,1,1,1,1,1,
1,1,1,1,1,1,1,1,
1,1,1,1,1,1,1,1,
1,1,1,1,1,1,1,1,
1,1,1,1,1,1,1,1,
1,1,1,1,1,1,1,1
</data>
 </layer>
 <objectgroup name="Object Layer 1">
  <object id="1" name="spawn" x="16" y="16" width="32" height="32"/>
 </objectgroup>
</map>
`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(tmx), 0o600); err != nil {
		t.Fatalf("could not write map fixture: %v", err)
	}
}

// newCacheDir builds an isolated temp directory holding a map and its atlas so
// tests never share in-flight state with one another or with the checked-in
// testdata maps.
func newCacheDir(t *testing.T) (dir, mapPath string) {
	t.Helper()

	dir = t.TempDir()
	writeMapFixture(t, dir, "map.tmx")
	// Reuse the checked-in 16x16-compatible atlas by copying a known-good PNG.
	pngBytes, err := os.ReadFile(filepath.Join("testdata", "singleWhite.png"))
	if err != nil {
		t.Fatalf("could not read source atlas: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "atlas.png"), pngBytes, 0o600); err != nil {
		t.Fatalf("could not write atlas fixture: %v", err)
	}
	return dir, filepath.Join(dir, "map.tmx")
}

// resetFlights clears any in-flight entries and restores the cleared state when
// the test finishes, so tests are isolated from one another.
func resetFlights(t *testing.T) {
	t.Helper()
	maps.mu.Lock()
	maps.flights = make(map[string]*inFlight)
	maps.mu.Unlock()

	t.Cleanup(func() {
		maps.mu.Lock()
		maps.flights = make(map[string]*inFlight)
		maps.mu.Unlock()
	})
}

func assertFixtureInvariant(t *testing.T, m *Map) {
	t.Helper()
	if m == nil {
		t.Fatal("map is nil")
	}
	if m.Bounds() != pixel.R(0, 0, 128, 128) {
		t.Fatalf("map bounds changed: %v", m.Bounds())
	}
	if m.GetTileLayerByName("Tile Layer 1") == nil {
		t.Fatal("tile layer missing")
	}
	objs := m.GetObjectByName("spawn")
	if len(objs) != 1 {
		t.Fatalf("expected 1 spawn object, got %d", len(objs))
	}
	r, err := objs[0].GetRect()
	if err != nil {
		t.Fatalf("GetRect failed: %v", err)
	}
	// Tiled y=16 in a 128px-tall map with a 32px object flips to y=80.
	if want := pixel.R(16, 80, 48, 112); r != want {
		t.Fatalf("flipped object rect = %v, want %v", r, want)
	}
}

func TestMapCache_SequentialReloadsAreIndependent(t *testing.T) {
	resetFlights(t)
	_, mapPath := newCacheDir(t)

	first, err := ReadFile(mapPath)
	if err != nil {
		t.Fatalf("first load failed: %v", err)
	}
	assertFixtureInvariant(t, first)

	second, err := ReadFile(mapPath)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	assertFixtureInvariant(t, second)

	// A sequential reload is a fresh load: the new map owns its own layers,
	// objects, batches and sprites, so nothing from the old map can pollute it.
	if first == second {
		t.Fatal("sequential reload returned the same map instance")
	}
	if first.GetTileLayerByName("Tile Layer 1") == second.GetTileLayerByName("Tile Layer 1") {
		t.Fatal("reloaded map shares a tile layer instance with the old map")
	}
	if first.GetObjectByName("spawn")[0] == second.GetObjectByName("spawn")[0] {
		t.Fatal("reloaded map shares an object instance with the old map")
	}

	maps.mu.Lock()
	flights := len(maps.flights)
	maps.mu.Unlock()
	if flights != 0 {
		t.Fatalf("completed loads left %d in-flight entries", flights)
	}
}

func TestMapCache_FailedLoadDoesNotDisturbEarlierMap(t *testing.T) {
	resetFlights(t)
	dir, mapPath := newCacheDir(t)

	good, err := ReadFile(mapPath)
	if err != nil {
		t.Fatalf("initial load failed: %v", err)
	}
	assertFixtureInvariant(t, good)

	// Break the atlas on disk: the next load of the same path must fail
	// because the external image is missing...
	if err := os.Remove(filepath.Join(dir, "atlas.png")); err != nil {
		t.Fatal(err)
	}

	bad, err := ReadFile(mapPath)
	if err == nil {
		t.Fatal("expected an error when the atlas is missing, got nil")
	}
	if bad != nil {
		t.Fatalf("failed load must not return a half-initialised map, got %v", bad)
	}

	// ...no in-flight state may linger...
	maps.mu.Lock()
	flights := len(maps.flights)
	maps.mu.Unlock()
	if flights != 0 {
		t.Fatal("failed load left an in-flight entry behind")
	}

	// ...and the previously returned map remains fully usable.
	assertFixtureInvariant(t, good)
}

func TestMapCache_FailureIsRetriedOnNextRequest(t *testing.T) {
	resetFlights(t)
	dir, mapPath := newCacheDir(t)

	if err := os.Remove(mapPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(mapPath); err == nil {
		t.Fatal("expected error loading a missing map file, got nil")
	}

	maps.mu.Lock()
	flights := len(maps.flights)
	maps.mu.Unlock()
	if flights != 0 {
		t.Fatal("failed load left an in-flight entry behind")
	}

	// Restore the file: the same path must load successfully instead of being
	// stuck in the failed state.
	writeMapFixture(t, dir, "map.tmx")
	m, err := ReadFile(mapPath)
	if err != nil {
		t.Fatalf("retry after failure did not succeed: %v", err)
	}
	assertFixtureInvariant(t, m)
}

func TestMapCache_ParseFailureRetriesSucceed(t *testing.T) {
	resetFlights(t)
	dir, mapPath := newCacheDir(t)

	if err := os.WriteFile(mapPath, []byte("<map><unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(mapPath); err == nil {
		t.Fatal("expected an XML parse error, got nil")
	}

	// A corrected file at the same path loads successfully on retry.
	writeMapFixture(t, dir, "map.tmx")
	m, err := ReadFile(mapPath)
	if err != nil {
		t.Fatalf("retry after parse failure did not succeed: %v", err)
	}
	assertFixtureInvariant(t, m)
}

// TestMapCache_LoadFileDeduplicatesConcurrentLoads exercises the single-flight
// machinery directly with an injected loader: however many callers pile in,
// the loader runs exactly once and every caller receives the same result.
func TestMapCache_LoadFileDeduplicatesConcurrentLoads(t *testing.T) {
	resetFlights(t)

	var loads int32
	block := make(chan struct{})
	want := &Map{Width: 42}
	load := func() (*Map, error) {
		atomic.AddInt32(&loads, 1)
		<-block // hold the first (and only) load open until callers have piled in
		return want, nil
	}

	const callers = 16
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]*Map, callers)
		errs    = make([]error, callers)
	)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = maps.loadFile("dedup/path.tmx", load)
		}(i)
	}
	close(start)
	time.Sleep(50 * time.Millisecond) // let every caller reach the in-flight load
	close(block)
	wg.Wait()

	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Fatalf("loader ran %d times, expected exactly 1", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d got error: %v", i, err)
		}
		if results[i] != want {
			t.Fatalf("caller %d received a different map", i)
		}
	}

	// A later, sequential call is a fresh load: results are never retained.
	again, err := maps.loadFile("dedup/path.tmx", load)
	if err != nil {
		t.Fatal(err)
	}
	if again != want {
		t.Fatal("sequential load returned an unexpected map")
	}
	if got := atomic.LoadInt32(&loads); got != 2 {
		t.Fatalf("sequential load was served from a retained result (loads=%d)", got)
	}
}

// TestMapCache_ConcurrentFailureReachesEveryCallerAndRetries ensures waiters
// on a shared failing load all receive the error and nil map, and that the
// next request retries rather than replaying the failure.
func TestMapCache_ConcurrentFailureReachesEveryCallerAndRetries(t *testing.T) {
	resetFlights(t)

	var loads int32
	sentinel := errors.New("boom")
	block := make(chan struct{})
	failingLoad := func() (*Map, error) {
		atomic.AddInt32(&loads, 1)
		<-block
		return nil, sentinel
	}

	const callers = 16
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, callers)
		got   = make([]*Map, callers)
	)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			got[idx], errs[idx] = maps.loadFile("failing/path.tmx", failingLoad)
		}(i)
	}
	close(start)
	time.Sleep(50 * time.Millisecond)
	close(block)
	wg.Wait()

	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Fatalf("loader ran %d times, expected 1", got)
	}
	for i, err := range errs {
		if !errors.Is(err, sentinel) {
			t.Fatalf("caller %d expected sentinel error, got %v", i, err)
		}
		if got[i] != nil {
			t.Fatalf("caller %d got a non-nil map on failure", i)
		}
	}

	// Retrying runs the loader again instead of replaying the cached failure.
	want := &Map{Width: 7}
	recovered, err := maps.loadFile("failing/path.tmx", func() (*Map, error) {
		atomic.AddInt32(&loads, 1)
		return want, nil
	})
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if recovered != want {
		t.Fatal("retry did not run a fresh load")
	}
	if got := atomic.LoadInt32(&loads); got != 2 {
		t.Fatalf("retry did not re-attempt the load (loads=%d)", got)
	}
}

func TestMapCache_ConcurrentReadFilesShareOneMap(t *testing.T) {
	resetFlights(t)
	_, mapPath := newCacheDir(t)

	const callers = 32
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]*Map, callers)
		errs    = make([]error, callers)
	)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = ReadFile(mapPath)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d got unexpected error: %v", i, err)
		}
	}
	for i := 1; i < callers; i++ {
		if results[i] != results[0] {
			t.Fatalf("caller %d received a different map instance than caller 0", i)
		}
	}
	assertFixtureInvariant(t, results[0])

	maps.mu.Lock()
	flights := len(maps.flights)
	maps.mu.Unlock()
	if flights != 0 {
		t.Fatalf("concurrent load left %d in-flight entries", flights)
	}
}

func TestMapCache_DifferentPathsDoNotShareInstances(t *testing.T) {
	resetFlights(t)
	_, path1 := newCacheDir(t)
	_, path2 := newCacheDir(t)

	m1, err := ReadFile(path1)
	if err != nil {
		t.Fatalf("load of first map failed: %v", err)
	}
	m2, err := ReadFile(path2)
	if err != nil {
		t.Fatalf("load of second map failed: %v", err)
	}
	if m1 == m2 {
		t.Fatal("different paths returned the same map instance")
	}
	if m1.GetTileLayerByName("Tile Layer 1") == m2.GetTileLayerByName("Tile Layer 1") {
		t.Fatal("different maps share a tile layer instance")
	}
	if m1.GetObjectByName("spawn")[0] == m2.GetObjectByName("spawn")[0] {
		t.Fatal("different maps share an object instance")
	}
}
