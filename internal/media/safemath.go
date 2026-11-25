package media

import "math"

// AddU64 returns a+b and reports whether the addition overflows uint64.
func AddU64(a, b uint64) (uint64, bool) {
	if b > math.MaxUint64-a {
		return 0, true
	}
	return a + b, false
}

// MulU64 returns a*b and reports whether the multiplication overflows uint64.
func MulU64(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	if b > math.MaxUint64/a {
		return 0, true
	}
	return a * b, false
}

// CheckBounds reports whether off+length fits within limit without
// overflow, i.e. a [off, off+length) read is inside [0, limit).
func CheckBounds(off, length, limit uint64) bool {
	end, overflow := AddU64(off, length)
	if overflow {
		return false
	}
	return end <= limit
}
