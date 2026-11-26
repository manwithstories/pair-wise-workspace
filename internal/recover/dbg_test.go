package recover

import (
	"testing"

	"audiorestore/internal/wav"
)

// TestBadPredicate makes the defect classification explicit: a block is
// repair-eligible only when it is missing, CRC-corrupt or truncated.
func TestBadPredicate(t *testing.T) {
	intact := wav.FrameBlock{Index: 0}
	if bad(intact) {
		t.Fatal("intact block must not count as bad")
	}
	states := []wav.FrameBlock{
		{Index: 1, Missing: true},
		{Index: 2, Corrupt: true},
		{Index: 3, Truncated: true},
	}
	for _, b := range states {
		if !bad(b) {
			t.Fatalf("block %d should be classified bad", b.Index)
		}
	}
	// A parity block that is otherwise intact is not itself a defect.
	parity := wav.FrameBlock{Index: 4, Parity: true}
	if bad(parity) {
		t.Fatal("an intact parity block must not be classified bad")
	}
}
