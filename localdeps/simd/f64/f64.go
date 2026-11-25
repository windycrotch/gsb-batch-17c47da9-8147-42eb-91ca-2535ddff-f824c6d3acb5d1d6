// Package f64 is a local offline stub of github.com/tphakala/simd/f64.
package f64

// Autocorrelate writes autoc[lag] = sum_{i=lag}^{N-1} x[i]*x[i-lag] for
// lags 0..maxLag inclusive (autoc must have len >= maxLag+1), matching the
// scalar reference autocorrelateInto in go-flac's lpc package bit-for-bit.
func Autocorrelate(autoc, x []float64, maxLag int) {
	n := len(x)
	for lag := 0; lag <= maxLag; lag++ {
		var sum float64
		for i := lag; i < n; i++ {
			sum += x[i] * x[i-lag]
		}
		autoc[lag] = sum
	}
}
