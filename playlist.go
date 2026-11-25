package hls

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Variant is HLS variant that gonna be use to generate HLS master playlist
type Variant struct {
	// URL indicate the location of the variant playlist.
	// If variant located on remote server, this url should
	// contain the full url
	URL string

	// Bandwidth is an integer that is the upper bound of
	// the overall bitrate for each media file, in bits per second
	Bandwidth string

	// Resolution is display size, in pixels, at which to display
	// all of the video in the playlist
	Resolution string

	// Codecs is quoted string containing a comma-separated list of formats,
	// where each format specifies a media sample type that's present
	// in a media segment in the playlist file. Valid format identifiers are
	// those in the ISO file format name space defined by RFC 6381
	Codecs string
}

// GenerateHLSVariant will generate variants info from the given resolutions.
// The available resolutions are: 360p, 480p, 720p and 1080p.
// An unknown resolution is reported as an error instead of being silently
// skipped.
func GenerateHLSVariant(resOptions []string, locPrefix string) ([]*Variant, error) {
	if len(resOptions) == 0 {
		return nil, errors.New("hls: please give at least 1 resolution")
	}

	variants := make([]*Variant, 0, len(resOptions))

	for _, r := range resOptions {
		c, err := getConfig(r)
		if err != nil {
			return nil, err
		}

		url := fmt.Sprintf("%s.m3u8", c.Name)
		if locPrefix != "" {
			url = locPrefix + "/" + url
		}

		variants = append(variants, &Variant{
			URL:        url,
			Bandwidth:  c.Bandwidth,
			Resolution: c.Resolution,
		})
	}

	return variants, nil
}

// GeneratePlaylist renders the master playlist content for the given variants.
func renderPlaylist(variants []*Variant) string {
	// M3U Header
	data := "#EXTM3U\n"
	data += "#EXT-X-VERSION:3\n"

	// Add M3U Info for each variant
	for _, v := range variants {
		// URL & bandwidth is required; skip incomplete entries rather than
		// emitting a broken master playlist line.
		if v.URL == "" || v.Bandwidth == "" {
			continue
		}

		data += "#EXT-X-STREAM-INF:"
		data += fmt.Sprintf("BANDWIDTH=%s", v.Bandwidth)
		if v.Resolution != "" {
			data += fmt.Sprintf(",RESOLUTION=%s", v.Resolution)
		}
		if v.Codecs != "" {
			data += fmt.Sprintf(",CODECS=%s", v.Codecs)
		}

		data += fmt.Sprintf("\n%s\n", v.URL)
	}

	return data
}

// GeneratePlaylist writes the master playlist file from the given variants
// into targetPath/filename (default "playlist.m3u8"). The write is atomic:
// content goes to a temporary file in the same directory and is renamed into
// place, so a reader can never observe a partially written master playlist.
func GeneratePlaylist(variants []*Variant, targetPath, filename string) error {
	if filename == "" {
		filename = "playlist.m3u8"
	}

	if err := os.MkdirAll(targetPath, 0o755); err != nil {
		return err
	}

	finalPath := filepath.Join(targetPath, filename)

	f, err := os.CreateTemp(targetPath, ".playlist-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := f.WriteString(renderPlaylist(variants)); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		cleanup()
		return err
	}
	return nil
}
