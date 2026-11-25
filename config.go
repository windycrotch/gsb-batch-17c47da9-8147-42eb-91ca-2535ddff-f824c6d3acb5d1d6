package hls

import "errors"

type config struct {
	Name         string
	Height       int
	VideoBitrate string
	Maxrate      string
	BufSize      string
	AudioBitrate string
	Resolution   string
	Bandwidth    string
}

var preset = map[string]*config{
	"360p": {
		Name:         "360p",
		Height:       360,
		VideoBitrate: "800k",
		Maxrate:      "856k",
		BufSize:      "1200k",
		AudioBitrate: "96k",
		Resolution:   "640x360",
		Bandwidth:    "800000",
	},
	"480p": {
		Name:         "480p",
		Height:       480,
		VideoBitrate: "1400k",
		Maxrate:      "1498k",
		BufSize:      "2100k",
		AudioBitrate: "128k",
		Resolution:   "842x480",
		Bandwidth:    "1400000",
	},
	"720p": {
		Name:         "720p",
		Height:       720,
		VideoBitrate: "2800k",
		Maxrate:      "2996k",
		BufSize:      "4200k",
		AudioBitrate: "128k",
		Resolution:   "1280x720",
		Bandwidth:    "2800000",
	},
	"1080p": {
		Name:         "1080p",
		Height:       1080,
		VideoBitrate: "5000k",
		Maxrate:      "5350k",
		BufSize:      "7500k",
		AudioBitrate: "192k",
		Resolution:   "1920x1080",
		Bandwidth:    "5000000",
	},
}

// ErrUnknownResolution is returned when a requested resolution is not one of
// the supported presets (360p, 480p, 720p, 1080p).
var ErrUnknownResolution = errors.New("hls: unknown resolution preset")

// getConfig return config from the available preset.
// Unknown resolutions are reported as an error instead of being silently
// skipped.
func getConfig(res string) (*config, error) {
	cfg, ok := preset[res]
	if !ok {
		return nil, ErrUnknownResolution
	}

	return cfg, nil
}
