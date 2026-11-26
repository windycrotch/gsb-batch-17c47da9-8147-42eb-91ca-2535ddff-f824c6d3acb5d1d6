package tilepix

/*
  _______ _ _      _____ _
 |__   __(_) |    |  __ (_)
    | |   _| | ___| |__) |__  __
    | |  | | |/ _ \  ___/ \ \/ /
    | |  | | |  __/ |   | |>  <
    |_|  |_|_|\___|_|   |_/_/\_\
*/

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

const (
	gidHorizontalFlip = 0x80000000
	gidVerticalFlip   = 0x40000000
	gidDiagonalFlip   = 0x20000000
	gidFlip           = gidHorizontalFlip | gidVerticalFlip | gidDiagonalFlip
)

// ObjectType is used to represent the types an object can be.
type ObjectType int

func (o ObjectType) String() string {
	switch o {
	case EllipseObj:
		return "Ellipse"
	case PolygonObj:
		return "Polygon"
	case PolylineObj:
		return "Polyline"
	case RectangleObj:
		return "Rectangle"
	case PointObj:
		return "Point"
	case TileObj:
		return "Tile"
	}

	return "Unknown"
}

// These are the currently supported object types.
const (
	EllipseObj ObjectType = iota
	PolygonObj
	PolylineObj
	RectangleObj
	PointObj
	TileObj
)

// Errors which are returned from various places in the package.
var (
	ErrUnknownEncoding       = errors.New("tmx: invalid encoding scheme")
	ErrUnknownCompression    = errors.New("tmx: invalid compression method")
	ErrInvalidDecodedDataLen = errors.New("tmx: invalid decoded data length")
	ErrInvalidGID            = errors.New("tmx: invalid GID")
	ErrInvalidObjectType     = errors.New("tmx: the object type requested does not match this object")
	ErrInvalidPointsField    = errors.New("tmx: invalid points string")
	ErrInfiniteMap           = errors.New("tmx: infinite maps are not currently supported")
)

var (
	// NilTile is a tile with no tile set.  Will be skipped over when drawing.
	NilTile = &DecodedTile{Nil: true}
)

// GID is a global tile ID. Tiles can use GID or ID.
type GID uint32

// ID is a tile ID. Tiles can use GID or ID.
type ID uint32

// DataTile is a tile from a data object.
type DataTile struct {
	GID GID `xml:"gid,attr"`
}

func osOpen(name string) (http.File, error) {
	f, err := os.Open(name)
	return f, err
}

// Read will read, decode and initialise a Tiled Map from a data reader.
// openFileFunc is used to retrieve tilesets and can be nil, in which case os.Open is used.
func Read(r io.Reader, dir string, openFileFunc func(name string) (http.File, error)) (*Map, error) {
	log.Debug("Read: reading from io.Reader")
	if openFileFunc == nil {
		openFileFunc = osOpen
	}
	var m Map
	if err := xml.NewDecoder(r).Decode(&m); err != nil {
		log.WithError(err).Error("Read: could not decode to Map")
		return nil, err
	}

	m.dir = dir

	if m.Infinite {
		log.WithError(ErrInfiniteMap).Error("Read: map has attribute 'infinite=true', not supported")
		return nil, ErrInfiniteMap
	}

	log.WithField("Tileset count", len(m.Tilesets)).Debug("Read: checking for tileset sources")
	for i, ts := range m.Tilesets {
		if ts.Source != "" {
			f, err := openFileFunc(filepath.Join(dir, ts.Source))
			if err != nil {
				log.WithError(err).Error("Read: could not read tileset source")
				return nil, err
			}
			sourceTs, err := readTileset(f, dir)
			_ = f.Close()
			if err != nil {
				log.WithError(err).Error("Read: could not read tileset source")
				return nil, err
			}
			sourceTs.FirstGID = ts.FirstGID
			m.Tilesets[i] = sourceTs
		}
	}

	if err := m.decodeLayers(); err != nil {
		log.WithError(err).Error("Read: could not decode layers")
		return nil, err
	}

	m.setParents()

	log.WithField("TileLayer count", len(m.TileLayers)).Debug("Read: processing layer tilesets")
	for _, l := range m.TileLayers {
		tileset, isEmpty, usesMultipleTilesets := getTileset(l)
		if usesMultipleTilesets {
			log.Debug("Read: multiple tilesets in use")
			continue
		}
		l.Empty, l.Tileset = isEmpty, tileset
	}

	// Tiled calculates co-ordinates from the top-left, flipping the y co-ordinate means we match the standard
	// bottom-left calculation.
	log.WithField("Object layer count", len(m.ObjectGroups)).Debug("Read: processing object layers")
	for _, og := range m.ObjectGroups {
		og.flipY()
	}

	log.WithField("Tileset count", len(m.Tilesets)).Debug("Read: processing tilesets")
	// Preload every tileset sprite now, while the map is still being built.  A
	// missing or unreadable atlas is an error: returning it here means the
	// caller never receives a half-initialised map whose batches would fail
	// later at draw time.
	for _, ts := range m.Tilesets {
		if _, err := ts.setSprite(); err != nil {
			log.WithError(err).Error("Read: could not preload tileset sprite")
			return nil, err
		}
	}

	return &m, nil
}

// ReadFile will read, decode and initialise a Tiled Map from a file path.
//
// Concurrent calls for the same path are deduplicated (single-flight): only one
// load runs and every concurrent caller receives the same fully initialised
// map, or the same explicit error.  A load error is never retained, so a later
// call for the same path retries the load instead of being stuck in a failed
// state, and a failed load never disturbs a map which was returned successfully
// earlier.  Sequential calls each perform a fresh load whose batches and
// sprites are independent of the previous map, so no state leaks between maps.
func ReadFile(filePath string) (*Map, error) {
	log.WithField("Filepath", filePath).Debug("ReadFile: reading file")

	return maps.loadFile(filePath, func() (*Map, error) {
		return loadMapFromFile(filePath)
	})
}
