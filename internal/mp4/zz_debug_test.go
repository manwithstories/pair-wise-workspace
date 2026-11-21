package mp4

import "testing"

// TestUnknownBoxesCopiesTable guards the copy semantics of the helper: a
// caller mutating the returned slice must not corrupt the parse result.
func TestUnknownBoxesCopiesTable(t *testing.T) {
	f := &File{Unknown: []string{"wvnd"}}
	got := f.UnknownBoxes()
	got[0] = "mutated"
	if f.Unknown[0] != "wvnd" {
		t.Fatal("UnknownBoxes returned an aliased slice")
	}
}
