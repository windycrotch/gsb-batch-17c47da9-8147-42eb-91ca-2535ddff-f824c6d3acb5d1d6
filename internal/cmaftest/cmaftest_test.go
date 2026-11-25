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

package cmaftest

import (
	"bytes"
	"testing"

	mp4ff "github.com/Eyevinn/mp4ff/mp4"
)

func TestInitSegmentIsCMAF(t *testing.T) {
	data := Track{HandlerType: "vide", Timescale: 90000, Width: 1280, Height: 720}.InitSegment()

	sr, err := mp4ff.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if sr.Ftyp == nil {
		t.Fatal("missing ftyp")
	}
	if sr.Moov == nil || len(sr.Moov.Traks) != 1 {
		t.Fatal("expected exactly one trak")
	}
	if sr.Moov.Mvex == nil {
		t.Fatal("expected mvex for a CMAF header")
	}
	hdlr := sr.Moov.Traks[0].Mdia.Hdlr
	if hdlr == nil || hdlr.HandlerType != "vide" {
		t.Fatalf("unexpected hdlr: %+v", hdlr)
	}
	ts := sr.Moov.Traks[0].Mdia.Mdhd.Timescale
	if ts != 90000 {
		t.Fatalf("unexpected timescale %d", ts)
	}
}

func TestFragmentIsCMAF(t *testing.T) {
	data := FragmentOptions{
		SequenceNumber:      3,
		BaseMediaDecodeTime: 90000,
		SampleCount:         2,
		SampleDuration:      18000,
		SampleSize:          64,
		Sync:                true,
	}.Fragment()

	sr, err := mp4ff.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(sr.Segments) != 1 {
		t.Fatalf("expected one segment, got %d", len(sr.Segments))
	}
	frag := sr.Segments[0].Fragments[0]
	if frag.Moof == nil || frag.Mdat == nil {
		t.Fatal("expected moof and mdat")
	}
	if got := frag.Moof.Mfhd.SequenceNumber; got != 3 {
		t.Fatalf("unexpected sequence number %d", got)
	}
	if got := frag.Moof.Traf.Tfdt.BaseMediaDecodeTime(); got != 90000 {
		t.Fatalf("unexpected base media decode time %d", got)
	}
	if n := frag.Moof.Traf.Trun.SampleCount(); n != 2 {
		t.Fatalf("unexpected sample count %d", n)
	}

	flags, ok := frag.Moof.Traf.Trun.FirstSampleFlags()
	if !ok {
		// fall back to per-sample flags
		flags = frag.Moof.Traf.Trun.Samples[0].Flags
	}
	if !mp4ff.IsSyncSampleFlags(flags) {
		t.Fatal("expected a sync sample")
	}
}
