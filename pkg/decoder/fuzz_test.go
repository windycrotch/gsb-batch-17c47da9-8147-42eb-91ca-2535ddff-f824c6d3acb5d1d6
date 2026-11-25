package decoder

import (
	"math/rand"
	"testing"
)

// TestDecodeFrameRandomInputNeverPanics feeds random and structured-random
// bytes to the decoder. A corrupt stream must surface as an error, never as a
// panic, so the CLI can skip the frame and keep going.
func TestDecodeFrameRandomInputNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for iter := 0; iter < 30000; iter++ {
		dec := New()
		channels := 1 + rng.Intn(6)
		if iter%4 != 0 {
			// Configure via ASC with a random (valid) sample index.
			if err := dec.SetASC(buildASC(2, rng.Intn(12), channels)); err != nil {
				continue
			}
		}

		for f := 0; f < 1+rng.Intn(3); f++ {
			var frame []byte
			switch rng.Intn(3) {
			case 0:
				// Arbitrary bytes with no ADTS structure.
				frame = make([]byte, rng.Intn(64))
				rng.Read(frame)
			default:
				// ADTS header (possibly inconsistent) plus random payload.
				payload := make([]byte, 1+rng.Intn(300))
				rng.Read(payload)
				frame = randomADTSHeader(rng, 7+rng.Intn(120))
				frame = append(frame, payload...)
			}

			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panic on iter=%d frame=%x: %v", iter, frame, r)
					}
				}()
				_, _ = dec.DecodeFrame(frame)
			}()
		}
	}
}

func randomADTSHeader(rng *rand.Rand, declaredLength int) []byte {
	w := newBitWriter()
	w.write(12, 0xfff)
	w.write(1, 0)                    // MPEG-4
	w.write(2, 0)                    // layer
	w.write(1, uint32(rng.Intn(2)))  // protection absent
	w.write(2, uint32(rng.Intn(4)))  // profile
	w.write(4, uint32(rng.Intn(16))) // sampling index
	w.write(1, 0)                    // private
	w.write(3, uint32(rng.Intn(8)))  // channel config
	w.write(4, 0)                    // original/copy/home/copyright
	w.write(13, uint32(declaredLength))
	w.write(11, 0)
	w.write(2, 0)
	return w.bytes()
}
