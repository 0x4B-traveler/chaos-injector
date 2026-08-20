package fault

import "testing"

// TestSeededRngCanonicalSequence pins the first values of seed 42. The same
// constants are pinned in the Python test suite, so both implementations are
// proven to produce the identical random sequence (cross-language replay).
func TestSeededRngCanonicalSequence(t *testing.T) {
	want := []uint64{
		3580622183945639842,
		10378725325292465923,
		8967075514996744559,
		5001014893397904463,
		14825054885549601002,
		10736401887688096443,
	}
	rng := NewSeededRng(42)
	for i, w := range want {
		if got := rng.Next(); got != w {
			t.Fatalf("Next()[%d] = %d, want %d", i, got, w)
		}
	}
}

// TestSeededRngDeterministic verifies that the same seed always produces the
// same sequence, including the seed 0 edge case (splitmix64 maps it to a
// non-zero state).
func TestSeededRngDeterministic(t *testing.T) {
	for _, seed := range []int64{42, 0, 1, 123456789} {
		a, b := NewSeededRng(seed), NewSeededRng(seed)
		for i := 0; i < 20; i++ {
			if av, bv := a.Next(), b.Next(); av != bv {
				t.Fatalf("seed=%d: sequence diverged at %d (%d vs %d)", seed, i, av, bv)
			}
		}
	}
}

// TestSeededRngRanges verifies the range helpers stay within bounds and
// agree with the canonical Python values for seed 42.
func TestSeededRngRanges(t *testing.T) {
	rng := NewSeededRng(42)
	wantIntRange := []int{79, 97, 110, 222, 89, 230}
	for i, w := range wantIntRange {
		if got := rng.IntRange(64, 256); got != w {
			t.Fatalf("IntRange(64,256)[%d] = %d, want %d", i, got, w)
		}
	}

	rng = NewSeededRng(42)
	wantIntN := []int{1, 1, 2, 1, 2, 0} // index into ["cpu", "mem", "port"]
	for i, w := range wantIntN {
		if got := rng.IntN(3); got != w {
			t.Fatalf("IntN(3)[%d] = %d, want %d", i, got, w)
		}
	}

	// Bounds sanity across many draws.
	rng = NewSeededRng(7)
	for i := 0; i < 100; i++ {
		if v := rng.IntRange(30, 90); v < 30 || v > 90 {
			t.Fatalf("IntRange(30,90) out of bounds: %d", v)
		}
		if v := rng.IntN(5); v < 0 || v >= 5 {
			t.Fatalf("IntN(5) out of bounds: %d", v)
		}
	}
	if v := rng.IntRange(5, 5); v != 5 {
		t.Fatalf("IntRange(5,5) = %d, want 5", v)
	}
}

// TestSnapshotBasicFields verifies the reproducibility snapshot always
// carries identity fields; Linux-specific fields are checked when present.
func TestSnapshotBasicFields(t *testing.T) {
	s := Capture()
	if s.Ts == "" || s.Hostname == "" || s.Platform == "" || s.CPUCount == 0 {
		t.Fatalf("snapshot missing basic fields: %+v", s)
	}
	if len(s.LoadAvg) > 0 && len(s.LoadAvg) != 3 {
		t.Fatalf("loadavg must be empty or 3 values: %+v", s.LoadAvg)
	}
}
