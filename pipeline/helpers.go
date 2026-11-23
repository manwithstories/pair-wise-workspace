package pipeline

import (
	"fmt"

	"mediad/timeline"
)

// sizeOfFrameItem is the documented bounded estimate for one queued frame.
// The real struct holds an int64, a string header (16 bytes) and a bool; 64
// bytes is a conservative upper bound used only for peak-memory reporting.
const sizeOfFrameItem = 64

// timelineMillis rebuilds the rational unified timestamp from whole millis.
// Frame items cross the queue on an integer tick to keep queue elements small
// and allocation-free; alignment converts back to exact rationals.
func timelineMillis(ms int64) timeline.Rat {
	return timeline.FromMillis(ms)
}

// segmentPayload produces deterministic, self-describing segment bytes. The
// content is a function of (representation, segment index, cut frame) so two
// runs — full or resumed — always hash identically for the same manifest.
func segmentPayload(rep string, segIdx, frameIdx int) []byte {
	return []byte(fmt.Sprintf("MEDIAD-SEG rep=%s segment=%d frame=%d\n", rep, segIdx, frameIdx))
}
