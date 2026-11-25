package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/skrashevich/go-aac/pkg/adts"
	"github.com/skrashevich/go-aac/pkg/decoder"
)

// decodeStats summarises the outcome of an input file decode.
type decodeStats struct {
	sampleRate    int
	numChannels   int
	frameCount    int // valid frames decoded
	badFrames     int // frames skipped because they could not be decoded
	firstBadOff   int // byte offset of the first bad frame, -1 if none
	firstBadFrame int // 0-based index of the first bad frame, -1 if none
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. It returns the process exit code.
func run(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aac2pcm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	raw := fs.Bool("raw", false, "output raw PCM int16 LE to stdout instead of WAV")
	verbose := fs.Bool("v", false, "verbose: print sample rate, channels, frame count")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: aac2pcm [options] input.aac [output.wav]\n\n")
		fmt.Fprintf(stderr, "Decode AAC (ADTS) file to PCM. Corrupt frames are skipped.\n\n")
		fmt.Fprintf(stderr, "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nIf --raw is set, PCM int16 LE is written to stdout and output file is not needed.\n")
	}
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	args := fs.Args()
	if len(args) < 1 {
		fs.Usage()
		return 1
	}

	inputPath := args[0]
	var outputPath string
	if !*raw {
		if len(args) < 2 {
			fmt.Fprintf(stderr, "Error: output file required (or use --raw for stdout)\n")
			return 1
		}
		outputPath = args[1]
	}

	data, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error reading input: %v\n", err)
		return 1
	}

	if !adts.Probe(data) {
		fmt.Fprintf(stderr, "Error: input file does not contain ADTS data\n")
		return 1
	}

	samples, stats, err := decodeAll(data)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// No usable audio: fail without producing an output file.
	if stats.frameCount == 0 {
		fmt.Fprintf(stderr, "Error: no decodable frames found (%d bad frame(s))\n", stats.badFrames)
		return 1
	}

	// Defensive: the decoder always emits exactly FrameLength*channels floats
	// per valid frame. Derive the channel count from the actual sample layout so
	// the WAV header can never disagree with the payload.
	if stats.numChannels <= 0 {
		fmt.Fprintf(stderr, "Error: could not determine output channel count\n")
		return 1
	}

	if *verbose {
		fmt.Fprintf(stderr, "Sample rate:  %d Hz\n", stats.sampleRate)
		fmt.Fprintf(stderr, "Channels:     %d\n", stats.numChannels)
		fmt.Fprintf(stderr, "Frames:       %d\n", stats.frameCount)
		fmt.Fprintf(stderr, "Total samples: %d (per channel: %d)\n", len(samples), len(samples)/stats.numChannels)
		if stats.badFrames > 0 {
			fmt.Fprintf(stderr, "Bad frames:   %d (first at offset %d, frame %d)\n",
				stats.badFrames, stats.firstBadOff, stats.firstBadFrame)
		} else {
			fmt.Fprintf(stderr, "Bad frames:   0\n")
		}
	}

	int16Samples := floatToInt16(samples)

	if *raw {
		if err := binary.Write(stdout, binary.LittleEndian, int16Samples); err != nil {
			fmt.Fprintf(stderr, "Error writing raw PCM: %v\n", err)
			return 1
		}
		return 0
	}

	f, err := os.Create(outputPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error creating output file: %v\n", err)
		return 1
	}
	defer f.Close()

	if err := writeWAV(f, int16Samples, stats.sampleRate, stats.numChannels); err != nil {
		fmt.Fprintf(stderr, "Error writing WAV: %v\n", err)
		return 1
	}

	if *verbose {
		fmt.Fprintf(stderr, "Written: %s\n", outputPath)
	}
	return 0
}

// decodeAll decodes every frame it can find in data.
//
// Frame boundaries are driven by the ADTS headers. When a header is unreadable
// or declares an impossible/incomplete frame, the scanner resynchronises by
// advancing one byte at a time until the next plausible ADTS header; each such
// damaged region is counted once. When a well-formed frame fails to decode, the
// whole declared frame is skipped and counted once. In both cases the byte
// offset of the frame is recorded and decoding continues with the following
// frame.
//
// A fatal decoder configuration error stops decoding and is returned, so the
// caller never mistakes undecodable frames for corrupt ones.
func decodeAll(data []byte) ([]float32, decodeStats, error) {
	dec := decoder.New()
	stats := decodeStats{firstBadOff: -1, firstBadFrame: -1}

	var allSamples []float32
	offset := 0
	frameIndex := 0
	// inBadRun is true while skipping a stretch of bytes that do not form a
	// usable frame, so a damaged region is reported only once.
	inBadRun := false

	for offset < len(data) {
		hdr, ok := readValidHeader(data, offset)
		if !ok {
			if !inBadRun {
				recordBad(&stats, offset, frameIndex)
				inBadRun = true
			}
			offset++
			continue
		}
		inBadRun = false

		frameData := data[offset : offset+hdr.FrameLength]
		samples, err := dec.DecodeFrame(frameData)
		if err != nil {
			if !isFrameError(err) {
				return allSamples, stats, fmt.Errorf("decoder configuration problem at offset %d: %w", offset, err)
			}
			recordBad(&stats, offset, frameIndex)
			offset += hdr.FrameLength
			frameIndex++
			continue
		}

		if stats.frameCount == 0 {
			// Lock the output format to the first successfully decoded frame.
			stats.sampleRate = dec.Config.SampleRate
			if dec.Config.FrameLength > 0 && len(samples)%dec.Config.FrameLength == 0 {
				stats.numChannels = len(samples) / dec.Config.FrameLength
			}
		}
		allSamples = append(allSamples, samples...)
		stats.frameCount++
		offset += hdr.FrameLength
		frameIndex++
	}

	return allSamples, stats, nil
}

// readValidHeader parses an ADTS header at offset and confirms it describes a
// complete frame that fits in data.
func readValidHeader(data []byte, offset int) (adts.Header, bool) {
	if len(data)-offset < 7 {
		return adts.Header{}, false
	}
	hdr, err := adts.ReadHeaderFromBytes(data[offset:])
	if err != nil {
		return adts.Header{}, false
	}
	minLength := 7
	if !hdr.ProtectionAbsent {
		minLength = 9
	}
	if hdr.FrameLength < minLength {
		return adts.Header{}, false
	}
	if offset+hdr.FrameLength > len(data) {
		return adts.Header{}, false
	}
	return hdr, true
}

// isFrameError reports whether err describes a single corrupt frame (as opposed
// to a fatal decoder configuration problem).
func isFrameError(err error) bool {
	return errors.Is(err, decoder.ErrInvalidFrame)
}

func recordBad(stats *decodeStats, offset, frameIndex int) {
	stats.badFrames++
	if stats.firstBadOff < 0 {
		stats.firstBadOff = offset
		stats.firstBadFrame = frameIndex
	}
}

func clamp(v float64, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

func floatToInt16(samples []float32) []int16 {
	out := make([]int16, len(samples))
	for i, v := range samples {
		out[i] = int16(clamp(float64(v)*32767.0, -32768, 32767))
	}
	return out
}

func writeWAV(w io.Writer, samples []int16, sampleRate, numChannels int) error {
	dataSize := uint32(len(samples) * 2)
	fileSize := 36 + dataSize

	bitsPerSample := uint16(16)
	blockAlign := uint16(numChannels) * bitsPerSample / 8
	byteRate := uint32(sampleRate) * uint32(blockAlign)

	// RIFF header
	if _, err := w.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, fileSize); err != nil {
		return err
	}
	if _, err := w.Write([]byte("WAVE")); err != nil {
		return err
	}

	// fmt sub-chunk
	if _, err := w.Write([]byte("fmt ")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(16)); err != nil { // sub-chunk size
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(1)); err != nil { // PCM format
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(numChannels)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, byteRate); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, blockAlign); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, bitsPerSample); err != nil {
		return err
	}

	// data sub-chunk
	if _, err := w.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, dataSize); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, samples); err != nil {
		return err
	}

	return nil
}
