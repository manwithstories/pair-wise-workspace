package testkit

import (
	"fmt"

	"mediarun/internal/model"
)

// BurstDropRanges reports which packet indexes a burst scenario removes, so
// tests can assert the expected gap count.
func BurstDropRanges(count int) [][2]int {
	var out [][2]int
	for i := 0; i < count; i++ {
		if i%11 == 5 {
			end := i + 2
			if end >= count {
				end = count - 1
			}
			out = append(out, [2]int{i, end})
			i += 2
		}
	}
	return out
}

// CorruptBoxes damages an encoded movie in the ways the parser must reject.
// Each returned byte slice is a separate malformed input.
func CorruptBoxes(data []byte) ([][]byte, error) {
	if len(data) < 16 {
		return nil, fmt.Errorf("%w: movie too short to corrupt", model.ErrInvalid)
	}
	var out [][]byte
	// 1. Truncated file.
	out = append(out, append([]byte(nil), data[:len(data)/2]...))
	// 2. A top-level box with an impossible size.
	bad := append([]byte(nil), data...)
	bad[0], bad[1], bad[2], bad[3] = 0, 0, 0, 2
	out = append(out, bad)
	// 3. A box that claims more bytes than the file holds.
	bad = append([]byte(nil), data...)
	bad[0], bad[1], bad[2], bad[3] = 0x7F, 0xFF, 0xFF, 0xFF
	out = append(out, bad)
	// 4. A large-size header cut in half.
	bad = append([]byte(nil), data...)
	bad[0], bad[1], bad[2], bad[3] = 0, 0, 0, 1
	out = append(out, bad[:12])
	return out, nil
}
