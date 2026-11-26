package tilepix_test

import (
	"image/color"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "image/png"

	"github.com/bcvery1/tilepix"
	"github.com/gopxl/pixel"
	"github.com/gopxl/pixel/pixelgl"
)

// writeBehaviourMap writes a loadable TMX map into dir.  The map contains a
// fully populated tile layer and an object layer with one rectangle so that
// both visible tiles and object co-ordinates can be asserted after loading.
func writeBehaviourMap(t *testing.T, dir string) {
	t.Helper()

	const tmx = `<?xml version="1.0" encoding="UTF-8"?>
<map version="1.0" orientation="orthogonal" width="8" height="8" tilewidth="16" tileheight="16">
 <tileset firstgid="1" name="atlas" tilewidth="16" tileheight="16" tilecount="1" columns="1">
  <image source="atlas.png" width="16" height="16"/>
  <tile id="0">
   <objectgroup>
    <object id="1" x="0" y="0" width="16" height="16"/>
   </objectgroup>
  </tile>
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
  <object id="2" name="spawn" x="16" y="16" width="32" height="32"/>
 </objectgroup>
</map>
`
	if err := os.WriteFile(filepath.Join(dir, "map.tmx"), []byte(tmx), 0o600); err != nil {
		t.Fatalf("could not write map fixture: %v", err)
	}

	pngBytes, err := os.ReadFile(filepath.Join("testdata", "singleWhite.png"))
	if err != nil {
		t.Fatalf("could not read atlas fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "atlas.png"), pngBytes, 0o600); err != nil {
		t.Fatalf("could not write atlas fixture: %v", err)
	}
}

func behaviourMap(t *testing.T) (mapPath string) {
	t.Helper()
	dir := t.TempDir()
	writeBehaviourMap(t, dir)
	return filepath.Join(dir, "map.tmx")
}

// assertMapInvariant verifies that the visible tile layer and the flipped
// object co-ordinates describe the same, fully loaded map.
func assertMapInvariant(t *testing.T, m *tilepix.Map) {
	t.Helper()

	if m == nil {
		t.Fatal("map is nil")
	}
	if m.Bounds() != pixel.R(0, 0, 128, 128) {
		t.Fatalf("map bounds changed: %v", m.Bounds())
	}
	if layer := m.GetTileLayerByName("Tile Layer 1"); layer == nil {
		t.Fatal("tile layer missing")
	}
	if layer := m.GetObjectLayerByName("Object Layer 1"); layer == nil {
		t.Fatal("object layer missing")
	}

	objs := m.GetObjectByName("spawn")
	if len(objs) != 1 {
		t.Fatalf("expected 1 spawn object, got %d", len(objs))
	}
	// Tiled y=16 in a 128px-tall map with a 32px object flips to y=80.
	rect, err := objs[0].GetRect()
	if err != nil {
		t.Fatalf("GetRect failed: %v", err)
	}
	if want := pixel.R(16, 80, 48, 112); rect != want {
		t.Fatalf("flipped object rect = %v, want %v", rect, want)
	}
}

func TestReadFile_ReloadKeepsTilesAndObjectsConsistent(t *testing.T) {
	mapPath := behaviourMap(t)

	first, err := tilepix.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("first load failed: %v", err)
	}
	assertMapInvariant(t, first)

	// A sequential reload is a fresh load.  The new map must be a separate
	// instance with its own layers and objects, so batches and sprites from the
	// previous map can never pollute it - yet both maps must keep the visible
	// tiles and the flipped object co-ordinates consistent.
	second, err := tilepix.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if first == second {
		t.Fatal("sequential reload returned the same map instance")
	}
	if first.GetTileLayerByName("Tile Layer 1") == second.GetTileLayerByName("Tile Layer 1") {
		t.Fatal("reloaded map shares a tile layer instance with the old map")
	}
	if first.GetObjectByName("spawn")[0] == second.GetObjectByName("spawn")[0] {
		t.Fatal("reloaded map shares an object instance with the old map")
	}
	assertMapInvariant(t, second)

	// Both maps must independently draw and answer queries.  Drawing the new
	// map must not disturb the old one.
	target, err := pixelgl.NewWindow(pixelgl.WindowConfig{Bounds: pixel.R(0, 0, 100, 100)})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.DrawAll(target, color.Transparent, pixel.IM); err != nil {
		t.Fatalf("DrawAll on reloaded map failed: %v", err)
	}
	if err := second.GenerateTileObjectLayer(); err != nil {
		t.Fatalf("GenerateTileObjectLayer on reloaded map failed: %v", err)
	}
	if layer := second.GetObjectLayerByName("atlas-objectgroup"); layer == nil {
		t.Fatal("generated tile object layer missing on reloaded map")
	}
	if err := first.DrawAll(target, color.Transparent, pixel.IM); err != nil {
		t.Fatalf("DrawAll on the previous map failed after reload: %v", err)
	}
	assertMapInvariant(t, first)
}

func TestReadFile_MissingAtlasReturnsErrorAndKeepsPreviousMap(t *testing.T) {
	dir := t.TempDir()
	writeBehaviourMap(t, dir)
	mapPath := filepath.Join(dir, "map.tmx")

	good, err := tilepix.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("initial load failed: %v", err)
	}
	assertMapInvariant(t, good)

	if err := os.Remove(filepath.Join(dir, "atlas.png")); err != nil {
		t.Fatal(err)
	}

	// A fresh load with a missing atlas must fail and must not hand back a
	// half-initialised map.
	bad, err := tilepix.ReadFile(mapPath)
	if err == nil {
		t.Fatal("expected error when the external atlas is missing")
	}
	if bad != nil {
		t.Fatalf("expected nil map on failure, got %v", bad)
	}

	// The previously loaded map remains complete and usable.
	assertMapInvariant(t, good)
	target, err := pixelgl.NewWindow(pixelgl.WindowConfig{Bounds: pixel.R(0, 0, 100, 100)})
	if err != nil {
		t.Fatal(err)
	}
	if err := good.DrawAll(target, color.Transparent, pixel.IM); err != nil {
		t.Fatalf("previous map stopped drawing after a failed load: %v", err)
	}
}

func TestReadFile_RecoversAfterFailure(t *testing.T) {
	dir := t.TempDir()
	writeBehaviourMap(t, dir)
	mapPath := filepath.Join(dir, "map.tmx")

	// Corrupt the map, confirm the failure is reported...
	if err := os.WriteFile(mapPath, []byte("<map><unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tilepix.ReadFile(mapPath); err == nil {
		t.Fatal("expected parse error, got nil")
	}

	// ...then restore it and confirm retrying the same path succeeds.
	writeBehaviourMap(t, dir)
	m, err := tilepix.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("retry after failure failed: %v", err)
	}
	assertMapInvariant(t, m)
}

func TestReadFile_ConcurrentSamePathLoadsAreConsistent(t *testing.T) {
	mapPath := behaviourMap(t)

	const callers = 32
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]*tilepix.Map, callers)
		errs    = make([]error, callers)
	)

	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = tilepix.ReadFile(mapPath)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d got error: %v", i, err)
		}
		if results[i] == nil {
			t.Fatalf("caller %d got nil map", i)
		}
	}
	// Every caller must see the same complete map - never another map's
	// layers or objects.
	for i := 1; i < callers; i++ {
		if results[i] != results[0] {
			t.Fatalf("caller %d got a different map instance", i)
		}
	}
	assertMapInvariant(t, results[0])
}

func TestReadFile_ConcurrentLoadsOfDifferentPathsStayIsolated(t *testing.T) {
	pathA := behaviourMap(t)
	pathB := behaviourMap(t)

	const callers = 16
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]*tilepix.Map, callers)
		errs    = make([]error, callers)
	)

	wg.Add(callers)
	for i := 0; i < callers; i++ {
		path := pathA
		if i%2 == 1 {
			path = pathB
		}
		go func(idx int, p string) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = tilepix.ReadFile(p)
		}(i, path)
	}
	close(start)
	wg.Wait()

	ma, mb := results[0], results[1]
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d got error: %v", i, err)
		}
	}
	if ma == mb {
		t.Fatal("maps from different paths share an instance")
	}
	for i := 0; i < callers; i++ {
		want := ma
		if i%2 == 1 {
			want = mb
		}
		if results[i] != want {
			t.Fatalf("caller %d saw a map belonging to another path", i)
		}
		assertMapInvariant(t, results[i])
	}
}

func TestRead_MissingExternalTilesetReturnsError(t *testing.T) {
	dir := t.TempDir()
	const tmx = `<?xml version="1.0" encoding="UTF-8"?>
<map version="1.2" orientation="orthogonal" width="4" height="4" tilewidth="16" tileheight="16">
 <tileset firstgid="1" source="missing.tsx"/>
 <layer name="Tile Layer 1" width="4" height="4">
  <data encoding="csv">0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0</data>
 </layer>
</map>
`
	mapFile := filepath.Join(dir, "map.tmx")
	if err := os.WriteFile(mapFile, []byte(tmx), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(mapFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := tilepix.Read(f, dir, nil); err == nil {
		t.Fatal("expected error when external tileset is missing, got nil")
	}
}
