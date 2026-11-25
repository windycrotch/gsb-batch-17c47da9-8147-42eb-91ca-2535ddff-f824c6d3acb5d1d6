/*
Copyright 2022-2025 The nagare media authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package cmaftest provides helpers to build synthetic CMAF payloads for tests.
//
// The generated data is not meant to be played back: it is structurally valid CMAF (init segment + fragments) and can
// be ingested by the CMAF ingest app without relying on an external encoder.
package cmaftest

import (
	"bytes"
	"fmt"

	mp4ff "github.com/Eyevinn/mp4ff/mp4"
)

// Track describes a synthetic CMAF track.
type Track struct {
	// HandlerType is the hdlr handler type ("vide" or "soun").
	HandlerType string
	// Timescale is the media timescale (mdhd.Timescale).
	Timescale uint32
	// Width and Height are the display size for video tracks.
	Width, Height uint32
	// TrackID is the track ID used in the init segment. Defaults to 1.
	TrackID uint32
}

// InitSegment builds a CMAF initialization segment for the given track.
func (t Track) InitSegment() []byte {
	trackID := t.TrackID
	if trackID == 0 {
		trackID = 1
	}

	initSeg := mp4ff.CreateEmptyInit()
	trak := initSeg.AddEmptyTrack(t.Timescale, t.HandlerType, "und")
	trak.Tkhd.TrackID = trackID
	if t.HandlerType == "vide" {
		trak.Tkhd.Width = mp4ff.Fixed32(t.Width << 16)
		trak.Tkhd.Height = mp4ff.Fixed32(t.Height << 16)
	}

	buf := &bytes.Buffer{}
	if err := initSeg.Encode(buf); err != nil {
		panic(fmt.Sprintf("cmaftest: failed to encode init segment: %v", err))
	}
	return buf.Bytes()
}

// FragmentOptions describe one synthetic fragment.
type FragmentOptions struct {
	// SequenceNumber is the movie fragment sequence number.
	SequenceNumber uint32
	// BaseMediaDecodeTime is the decode time of the first sample.
	BaseMediaDecodeTime uint64
	// SampleCount is the number of samples in the fragment.
	SampleCount uint32
	// SampleDuration is the duration of each sample in the track timescale.
	SampleDuration uint32
	// SampleSize is the size of each sample payload in bytes.
	SampleSize uint32
	// Sync declares whether the fragment starts with a sync sample (keyframe). Fragments that do not start with a
	// sync sample belong to the preceding fragment.
	Sync bool
	// TrackID must match the init segment. Defaults to 1.
	TrackID uint32
}

// Fragment builds a single moof+mdat CMAF fragment.
func (o FragmentOptions) Fragment() []byte {
	trackID := o.TrackID
	if trackID == 0 {
		trackID = 1
	}
	if o.SampleCount == 0 {
		o.SampleCount = 1
	}

	frag, err := mp4ff.CreateFragment(o.SequenceNumber, trackID)
	if err != nil {
		panic(fmt.Sprintf("cmaftest: failed to create fragment: %v", err))
	}

	flags := mp4ff.NonSyncSampleFlags
	if o.Sync {
		flags = mp4ff.SyncSampleFlags
	}

	sampleData := bytes.Repeat([]byte{0xAB}, int(o.SampleSize))
	decodeTime := o.BaseMediaDecodeTime
	for i := uint32(0); i < o.SampleCount; i++ {
		frag.AddFullSampleToTrack(mp4ff.FullSample{
			Sample: mp4ff.Sample{
				Flags: flags,
				Dur:   o.SampleDuration,
				Size:  o.SampleSize,
			},
			DecodeTime: decodeTime,
			Data:       sampleData,
		}, trackID)
		decodeTime += uint64(o.SampleDuration)
	}

	buf := &bytes.Buffer{}
	if err := frag.Encode(buf); err != nil {
		panic(fmt.Sprintf("cmaftest: failed to encode fragment: %v", err))
	}
	return buf.Bytes()
}

// Chunk splits an encoded fragment at the given absolute offset so that it can be streamed as two separately flushed
// chunks. This is used to exercise the chunked/long-running upload path.
func Chunk(data []byte, splitAt int) (first, second []byte) {
	if splitAt <= 0 || splitAt >= len(data) {
		panic("cmaftest: splitAt out of range")
	}
	return data[:splitAt], data[splitAt:]
}
