package manifest

import (
	"bytes"
)

// sLineKey identifies an <S> element by representation id and its 0-based
// ordinal inside that representation's SegmentTimeline.
type sLineKey struct {
	repID string
	ord   int
}

// buildSegmentLines scans MPD text and records the 1-based source line of
// every <S ...> element, keyed by the representation enclosing it. XML
// attribute order is irrelevant: we track the nearest preceding id="...".
func buildSegmentLines(raw []byte) map[sLineKey]int {
	out := map[sLineKey]int{}
	repID := ""
	ordinal := 0
	for i, ln := range bytes.Split(raw, []byte("\n")) {
		line := string(ln)
		if id := attrAfter(line, "id="); id != "" && bytes.Contains(ln, []byte("Representation")) {
			repID = id
			ordinal = 0
		}
		if bytes.Contains(ln, []byte("<S ")) || bytes.Contains(ln, []byte("<S\t")) ||
			bytes.Contains(ln, []byte("<S>")) {
			if repID != "" {
				out[sLineKey{repID: repID, ord: ordinal}] = i + 1
				ordinal++
			}
		}
	}
	return out
}
