package hls

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateHLSVariant(t *testing.T) {
	t.Run("valid resolutions", func(t *testing.T) {
		variants, err := GenerateHLSVariant([]string{"360p", "480p"}, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(variants) != 2 {
			t.Fatalf("expected 2 variants, got %d", len(variants))
		}
	})

	t.Run("empty resolutions", func(t *testing.T) {
		if _, err := GenerateHLSVariant([]string{}, ""); err == nil {
			t.Fatal("expected error for empty resolutions")
		}
	})

	t.Run("unknown resolution is an error, not silently skipped", func(t *testing.T) {
		_, err := GenerateHLSVariant([]string{"360p", "invalid"}, "")
		if err == nil {
			t.Fatal("expected error for unknown resolution")
		}
		if !errorsIs(err, ErrUnknownResolution) {
			t.Fatalf("expected ErrUnknownResolution, got %v", err)
		}
	})

	t.Run("location prefix", func(t *testing.T) {
		variants, err := GenerateHLSVariant([]string{"720p"}, "prefix/dir")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if variants[0].URL != "prefix/dir/720p.m3u8" {
			t.Fatalf("unexpected url %q", variants[0].URL)
		}
	})
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestGeneratePlaylist(t *testing.T) {
	dir := t.TempDir()
	variants, err := GenerateHLSVariant([]string{"480p", "720p"}, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := GeneratePlaylist(variants, dir, ""); err != nil {
		t.Fatalf("GeneratePlaylist failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.HasPrefix(content, "#EXTM3U\n") {
		t.Fatalf("playlist missing m3u header: %q", content)
	}
	if !strings.Contains(content, "480p.m3u8") || !strings.Contains(content, "720p.m3u8") {
		t.Fatalf("playlist missing variants: %q", content)
	}

	// No temporary files may be left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".playlist-") {
			t.Fatalf("temporary playlist file left behind: %s", e.Name())
		}
	}
}
