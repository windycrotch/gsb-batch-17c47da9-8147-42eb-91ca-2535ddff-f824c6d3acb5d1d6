// Package cpu is a local offline stub of github.com/tphakala/simd/cpu. Feature
// flags are always false so the pure-Go scalar kernels are selected everywhere;
// they are bit-identical to the SIMD paths.
package cpu

type x86Features struct {
	AVX  bool
	AVX2 bool
}

type arm64Features struct {
	NEON bool
}

var X86 x86Features
var ARM64 arm64Features
