package cmaf

import (
	"fmt"

	"mediaflow/internal/clock"
)

// resolveAvailability checks every sample against the mdat (or the buffer for
// moof-carried data). Samples outside the validated range are marked
// truncated and reported with their original container offset.
func (s *Scanner) resolveAvailability(buf []byte, frag *Fragment, mdat Box, segIdx int) {
	segEnd := uint64(len(buf))
	var payloadStart, payloadEnd uint64
	haveMdat := mdat.Type == "mdat" && mdat.Offset >= 0
	if haveMdat {
		payloadStart = uint64(mdat.Offset + mdat.HeaderSize)
		payloadEnd = uint64(mdat.End)
		if payloadEnd > segEnd {
			payloadEnd = segEnd
		}
	}
	for i := range frag.Samples {
		sm := &frag.Samples[i]
		if !haveMdat {
			// A sample may still point into this same buffer (base data
			// offset) when the segment has no trailing mdat.
			if sm.Offset+uint64(sm.Size) > segEnd || sm.Offset < uint64(mdat.Offset) {
				sm.Truncated = true
				sm.Available = 0
			} else {
				sm.Available = sm.Size
			}
		} else {
			if sm.Offset < payloadStart || sm.Offset+uint64(sm.Size) > payloadEnd {
				sm.Truncated = true
				if sm.Offset >= payloadStart && sm.Offset < payloadEnd {
					sm.Available = uint32(payloadEnd - sm.Offset)
				} else {
					sm.Available = 0
				}
			} else {
				sm.Available = sm.Size
			}
		}
		if sm.Truncated {
			frag.Problems = append(frag.Problems, clock.LocatedError{
				Kind: clock.ErrSampleOutOfBounds, Segment: segIdx, Offset: sm.Offset,
				TrackID: sm.TrackID,
				Detail:  fmt.Sprintf("sample %d size %d extends outside segment", sm.Index, sm.Size),
			})
		}
	}
}
