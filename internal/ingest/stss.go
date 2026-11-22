package ingest

import (
	"bytes"
	"fmt"

	mp4 "github.com/abema/go-mp4"
)

// readStss extracts the sync-sample table for the track at trak ordinal
// trakIdx. go-mp4's Probe does not surface stss, so the table is read here and
// cross-checked against the sample count during attachKeyframes.
func readStss(data []byte, trakIdx int) ([]uint32, error) {
	r := bytes.NewReader(data)
	bips, err := mp4.ExtractBoxesWithPayload(r, nil, []mp4.BoxPath{
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStss()},
	})
	if err != nil {
		return nil, &BoxError{Box: "stbl", Detail: err.Error()}
	}
	if len(bips) == 0 {
		return nil, nil // non-video tracks legitimately omit stss
	}
	if trakIdx >= len(bips) {
		return nil, &BoxError{Box: "stss", Detail: fmt.Sprintf(
			"stss count %d cannot index trak %d", len(bips), trakIdx)}
	}
	stss, ok := bips[trakIdx].Payload.(*mp4.Stss)
	if !ok {
		return nil, &BoxError{Box: "stss", Detail: "unexpected payload type"}
	}
	return stss.SampleNumber, nil
}
