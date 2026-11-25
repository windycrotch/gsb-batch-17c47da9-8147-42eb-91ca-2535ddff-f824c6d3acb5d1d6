# Local offline build dependency

This directory is **not part of the shipped library**. It is a minimal,
bit-identical scalar stand-in for the single external module the build needs,
`github.com/tphakala/simd`, wired through the `replace` directive in `../go.mod`.

## Why it exists

The evaluation environment has no network access and the Go module cache does
not contain `github.com/tphakala/simd@v1.14.0`, so an ordinary `go test ./...`
fails downloading it. The replace redirects that import at this local copy,
letting the build and the full test suite run offline.

## What it contains

Only the three symbols the codec actually imports, each matching the real
module's documented semantics:

- `crc.Checksum16`: FLAC CRC-16 (poly 0x8005, init 0, MSB-first, no reflection),
  via a scalar slice-by-8 table loop. Bit-identical to the SIMD implementation.
- `cpu.X86.AVX/AVX2` and `cpu.ARM64.NEON`: always false, so the codec selects its
  pure-Go scalar kernels. Those kernels are required to be bit-identical to the
  SIMD ones (design rule 2 in AGENTS.md), so output is unchanged.
- `f64.Autocorrelate`: the scalar autocorrelation reference that the package's
  own parity tests use as the bit-exact oracle for the SIMD kernel.

No package here is imported by name anywhere in the library; the redirect is
invisible to the source. To build against the real module on a networked
machine, delete the `replace` line in `go.mod` and remove this directory.
