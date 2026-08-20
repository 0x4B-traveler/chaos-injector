package fault

// SeededRng is a deterministic PRNG (xorshift64* with a splitmix64 seed
// mixer) shared with the Python implementation. The same seed produces the
// same sequence in both languages, so "random -seed N" reproduces the exact
// same experiment on either implementation (稳定复现).
type SeededRng struct {
	state uint64
}

// NewSeededRng returns a PRNG for the given seed (any int64, including 0).
func NewSeededRng(seed int64) *SeededRng {
	// splitmix64: map any seed to a non-zero state.
	z := uint64(seed) + 0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return &SeededRng{state: z ^ (z >> 31)}
}

// Next returns the next uint64 in the sequence.
func (r *SeededRng) Next() uint64 {
	s := r.state
	s ^= s >> 12
	s ^= s << 25
	s ^= s >> 27
	r.state = s
	return s * 0x2545F4914F6CDD1D
}

// IntN returns a value in [0, n); mirrors Python's SeededRng.choice index.
func (r *SeededRng) IntN(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.Next() % uint64(n))
}

// IntRange returns a value in [lo, hi] inclusive; mirrors randint(lo, hi).
func (r *SeededRng) IntRange(lo, hi int) int {
	return lo + r.IntN(hi-lo+1)
}
