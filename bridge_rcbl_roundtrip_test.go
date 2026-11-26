package goff_test

// End-to-end normal-path coverage for the AVIO bridge:
//   - NewReader over an *os.File demuxes/decodes testdata/sample.mp4 -> 23 frames
//   - NewWriter over memfile produces an MP4 that can be re-opened and decoded
//
// Identifiers carry the rcblrt prefix to avoid collisions with any other
// bridge test files.

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/itchio/goff"
	"github.com/itchio/goff/memfile"
)

func rcblrtMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A1 bridge-flavoured baseline: feed sample.mp4 exclusively through a
// goff.Reader (custom AVIOContext) and decode 23 video frames.
func TestRCBLRT_ReaderBridge23Frames(t *testing.T) {
	f, err := os.Open("testdata/sample.mp4")
	rcblrtMust(t, err)
	defer f.Close()

	st, err := f.Stat()
	rcblrtMust(t, err)

	r := goff.NewReader(f, st.Size())
	defer r.Free()

	fctx := goff.FormatAllocContext()
	defer fctx.Free()
	rcblrtMust(t, fctx.OpenReader(r, nil, nil))
	rcblrtMust(t, fctx.FindStreamInfo(nil))

	if fctx.NbStreams() != 2 {
		t.Fatalf("NbStreams=%d, want 2", fctx.NbStreams())
	}
	vst := fctx.Streams()[0]
	if got := vst.CodecParameters().CodecType().String(); got != "video" {
		t.Fatalf("stream 0 type=%q want video", got)
	}

	dec := vst.CodecParameters().CodecID().FindDecoder()
	if dec == nil {
		t.Fatal("h264 decoder not found")
	}
	dctx := dec.AllocContext3()
	defer dctx.Free()
	rcblrtMust(t, vst.CodecParameters().ToContext(dctx))
	rcblrtMust(t, dctx.Open2(dec, nil))

	frame := goff.FrameAlloc()
	defer frame.Free()

	numFrames := 0
	receive := func() {
		for {
			err := dctx.ReceiveFrame(frame)
			if err != nil {
				if goff.IsEAGAIN(err) || goff.IsEOF(err) {
					return
				}
				rcblrtMust(t, err)
			}
			numFrames++
		}
	}

	var pkt goff.Packet
	defer pkt.Unref()

	for {
		pkt.Init()
		err := fctx.ReadFrame(&pkt)
		if err != nil {
			if goff.IsEOF(err) {
				break
			}
			rcblrtMust(t, err)
		}
		if pkt.StreamIndex() != vst.Index() {
			continue
		}
		pkt.RescaleTs(vst.TimeBase(), dctx.TimeBase())
		rcblrtMust(t, dctx.SendPacket(&pkt))
		receive()
	}
	receive()

	if numFrames != 23 {
		t.Fatalf("decoded %d frames through Reader bridge, want 23", numFrames)
	}
	if err := r.Error(); err != nil {
		t.Fatalf("reader should have no sticky error on clean EOF, got %v", err)
	}
}

// A2: transcode the sample into memfile through goff.Writer and prove the
// resulting bytes are a readable MP4 (re-demux + decode).
func TestRCBLRT_MemfileWriterProducesReadableMP4(t *testing.T) {
	in, err := os.Open("testdata/sample.mp4")
	rcblrtMust(t, err)
	defer in.Close()
	st, err := in.Stat()
	rcblrtMust(t, err)

	inCtx := goff.FormatAllocContext()
	defer inCtx.Free()
	inReader := goff.NewReader(in, st.Size())
	defer inReader.Free()
	rcblrtMust(t, inCtx.OpenReader(inReader, nil, nil))

	vst := inCtx.Streams()[0]
	dec := vst.CodecParameters().CodecID().FindDecoder()
	dctx := dec.AllocContext3()
	defer dctx.Free()
	rcblrtMust(t, vst.CodecParameters().ToContext(dctx))
	rcblrtMust(t, dctx.Open2(dec, nil))

	mf := memfile.New()
	w := goff.NewWriter(mf)

	outFmt := goff.GuessFormat("mp4", "", "")
	if outFmt == nil {
		t.Fatal("mp4 muxer not found")
	}
	outCtx, err := goff.FormatAllocOutputContext2(outFmt, "", "")
	rcblrtMust(t, err)
	defer outCtx.Free()
	outCtx.SetWriter(w)

	ost := outCtx.NewStream(dec)
	ost.SetID(0)
	encCodec := goff.CodecID_H264.FindEncoder()
	if encCodec == nil {
		t.Skip("h264 encoder unavailable")
	}
	ectx := encCodec.AllocContext3()
	defer ectx.Free()
	ectx.SetCodecType(goff.MediaType_Video)
	ectx.SetCodecID(goff.CodecID_H264)
	ectx.SetPixelFormat(goff.PixelFormat_YUV420P)
	ost.SetTimeBase(goff.TIME_BASE_Q)
	ectx.SetTimeBase(goff.NewRational(1, 120))
	ectx.SetWidth(dctx.Width())
	ectx.SetHeight(dctx.Height())
	rcblrtMust(t, goff.OptSet(ectx.PrivData(), "preset", "ultrafast", goff.SearchFlags_CHILDREN))
	rcblrtMust(t, ectx.Open2(encCodec, nil))
	rcblrtMust(t, ost.CodecParameters().FromContext(ectx))
	rcblrtMust(t, outCtx.WriteHeader(nil))

	frame := goff.FrameAlloc()
	defer frame.Free()

	drainEncoder := func(last bool) {
		for {
			var op goff.Packet
			op.Init()
			err := ectx.ReceivePacket(&op)
			if err != nil {
				if goff.IsEOF(err) {
					return
				}
				if !last && goff.IsEAGAIN(err) {
					return
				}
				rcblrtMust(t, err)
			}
			op.SetStreamIndex(ost.Index())
			op.RescaleTs(ectx.TimeBase(), ost.TimeBase())
			rcblrtMust(t, outCtx.InterleavedWriteFrame(&op))
		}
	}

	var pkt goff.Packet
	defer pkt.Unref()
	for {
		pkt.Init()
		err := inCtx.ReadFrame(&pkt)
		if err != nil {
			if goff.IsEOF(err) {
				break
			}
			rcblrtMust(t, err)
		}
		if pkt.StreamIndex() != vst.Index() {
			continue
		}
		pkt.RescaleTs(vst.TimeBase(), dctx.TimeBase())
		rcblrtMust(t, dctx.SendPacket(&pkt))
		for {
			err := dctx.ReceiveFrame(frame)
			if err != nil {
				if goff.IsEAGAIN(err) || goff.IsEOF(err) {
					break
				}
				rcblrtMust(t, err)
			}
			pts := frame.Pts().Rescale(dctx.TimeBase(), ectx.TimeBase())
			if pts.IsNop() {
				pts = 0
			}
			frame.SetPts(pts)
			rcblrtMust(t, ectx.SendFrame(frame))
			drainEncoder(false)
		}
	}
	rcblrtMust(t, ectx.SendFrame(nil))
	drainEncoder(true)
	rcblrtMust(t, outCtx.WriteTrailer())
	rcblrtMust(t, w.Free())

	size, err := mf.Seek(0, io.SeekEnd)
	rcblrtMust(t, err)
	if size < 4096 {
		t.Fatalf("encoded mp4 only %d bytes; memfile writer did not persist data", size)
	}
	if _, err := mf.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	// Re-open the produced bytes through another Reader/FormatContext.
	probeReader := goff.NewReader(mf, size)
	defer probeReader.Free()
	probeCtx := goff.FormatAllocContext()
	defer probeCtx.Free()
	rcblrtMust(t, probeCtx.OpenReader(probeReader, nil, nil))
	rcblrtMust(t, probeCtx.FindStreamInfo(nil))

	name := probeCtx.InputFormat().Name()
	if !strings.Contains(name, "mov") && !strings.Contains(name, "mp4") {
		t.Fatalf("detected format %q, want mov/mp4", name)
	}
	if probeCtx.NbStreams() < 1 {
		t.Fatal("probe found no streams")
	}
	ps := probeCtx.Streams()[0]
	if ps.CodecParameters().CodecID() != goff.CodecID_H264 {
		t.Fatalf("probe codec=%v want h264", ps.CodecParameters().CodecID())
	}

	// Decode at least one frame from the re-opened file.
	pdec := ps.CodecParameters().CodecID().FindDecoder()
	if pdec == nil {
		t.Fatal("probe decoder not found")
	}
	pctx := pdec.AllocContext3()
	defer pctx.Free()
	rcblrtMust(t, ps.CodecParameters().ToContext(pctx))
	rcblrtMust(t, pctx.Open2(pdec, nil))
	pf := goff.FrameAlloc()
	defer pf.Free()

	decoded := 0
	var pp goff.Packet
	defer pp.Unref()
	for decoded == 0 {
		pp.Init()
		if err := probeCtx.ReadFrame(&pp); err != nil {
			t.Fatalf("read probe packet: %v", err)
		}
		if pp.StreamIndex() != ps.Index() {
			continue
		}
		if err := pctx.SendPacket(&pp); err != nil {
			t.Fatalf("probe send packet: %v", err)
		}
		if err := pctx.ReceiveFrame(pf); err == nil {
			decoded++
		} else if !goff.IsEAGAIN(err) {
			t.Fatalf("probe receive frame: %v", err)
		}
	}
	if decoded != 1 {
		t.Fatal("expected to decode at least one frame from generated mp4")
	}
}
