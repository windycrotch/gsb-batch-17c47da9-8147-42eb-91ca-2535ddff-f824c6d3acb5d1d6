package filterbank

import "testing"

func TestProcessZeroInputPreservesOverlap(t *testing.T) {
	sequences := []int{OnlyLongSequence, LongStartSequence, EightShortSequence, LongStopSequence}
	for _, seq := range sequences {
		fb, err := New(false, 1)
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}

		overlap := fb.overlaps[0]
		for i := range overlap {
			overlap[i] = float32(i%7) - 3
		}
		prevOverlap := make([]float32, len(overlap))
		copy(prevOverlap, overlap)

		input := make([]float32, fb.length)
		output := make([]float32, fb.length)
		info := WindowInfo{WindowSequence: seq, WindowShape: [2]int{0, 0}}

		if err := fb.Process(info, input, output, 0); err != nil {
			t.Fatalf("Process seq=%d failed: %v", seq, err)
		}

		for i := range output {
			if output[i] != prevOverlap[i] {
				t.Fatalf("seq=%d output[%d]=%f want %f", seq, i, output[i], prevOverlap[i])
			}
		}
		for i := range overlap {
			if overlap[i] != 0 {
				t.Fatalf("seq=%d overlap[%d]=%f want 0", seq, i, overlap[i])
			}
		}
	}
}

func TestProcessInvalidArgs(t *testing.T) {
	fb, err := New(false, 1)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	info := WindowInfo{WindowSequence: OnlyLongSequence, WindowShape: [2]int{0, 0}}
	if err := fb.Process(info, make([]float32, fb.length-1), make([]float32, fb.length), 0); err == nil {
		t.Fatal("expected error for short input")
	}
	if err := fb.Process(info, make([]float32, fb.length), make([]float32, fb.length-1), 0); err == nil {
		t.Fatal("expected error for short output")
	}
	if err := fb.Process(info, make([]float32, fb.length), make([]float32, fb.length), 1); err == nil {
		t.Fatal("expected error for invalid channel")
	}
}

func BenchmarkProcessOnlyLong(b *testing.B) {
	fb, err := New(false, 1)
	if err != nil {
		b.Fatalf("New failed: %v", err)
	}
	input := make([]float32, fb.length)
	output := make([]float32, fb.length)
	info := WindowInfo{WindowSequence: OnlyLongSequence, WindowShape: [2]int{0, 0}}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fb.Process(info, input, output, 0)
	}
}

func BenchmarkProcessEightShort(b *testing.B) {
	fb, err := New(false, 1)
	if err != nil {
		b.Fatalf("New failed: %v", err)
	}
	input := make([]float32, fb.length)
	output := make([]float32, fb.length)
	info := WindowInfo{WindowSequence: EightShortSequence, WindowShape: [2]int{0, 0}}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fb.Process(info, input, output, 0)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	fb, err := New(false, 2)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	for ch := range fb.overlaps {
		for i := range fb.overlaps[ch] {
			fb.overlaps[ch][i] = float32((ch+1)*i%17) - 8
		}
	}

	snap := fb.Snapshot()

	// Mutate the live state; the snapshot must be unaffected (deep copy).
	for ch := range fb.overlaps {
		for i := range fb.overlaps[ch] {
			fb.overlaps[ch][i] = 12345
		}
	}
	for ch := range snap {
		for i := range snap[ch] {
			if snap[ch][i] == 12345 {
				t.Fatalf("Snapshot aliased live state at [%d][%d]", ch, i)
			}
		}
	}

	fb.Restore(snap)
	for ch := range fb.overlaps {
		for i := range fb.overlaps[ch] {
			want := float32((ch+1)*i%17) - 8
			if fb.overlaps[ch][i] != want {
				t.Fatalf("Restore mismatch [%d][%d]=%f want %f", ch, i, fb.overlaps[ch][i], want)
			}
		}
	}
}

func TestRestoreNilResetsToSilence(t *testing.T) {
	fb, err := New(false, 1)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	for i := range fb.overlaps[0] {
		fb.overlaps[0][i] = 1
	}
	fb.Restore(nil)
	for i := range fb.overlaps[0] {
		if fb.overlaps[0][i] != 0 {
			t.Fatalf("overlap[%d]=%f want 0", i, fb.overlaps[0][i])
		}
	}
}
