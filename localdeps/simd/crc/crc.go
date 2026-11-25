// Package crc is a local offline stub of github.com/tphakala/simd/crc providing
// the single symbol the go-flac build needs: FLAC's CRC-16 (poly 0x8005, init 0,
// MSB-first, no reflection), computed with the scalar slice-by-8 table loop.
package crc

var table [256]uint16

func init() {
	for i := range 256 {
		c := uint16(i) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = (c << 1) ^ 0x8005
			} else {
				c <<= 1
			}
		}
		table[i] = c
	}
}

// Checksum16 returns the CRC-16 of p (init 0), bit-identical to the SIMD version.
func Checksum16(p []byte) uint16 {
	var c uint16
	for _, b := range p {
		c = (c << 8) ^ table[byte(c>>8)^b]
	}
	return c
}
