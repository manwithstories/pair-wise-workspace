package mp4

import (
	"bytes"
	"fmt"

	gomp4 "github.com/abema/go-mp4"
)

// GoMP4Sequence independently extracts the mfhd sequence number using the
// pinned github.com/abema/go-mp4 library. It is a conformance cross-check for
// the local bounds-checked parser and the generated fixtures.
func GoMP4Sequence(raw []byte) (uint32, error) {
	boxes, err := gomp4.ExtractBoxWithPayload(
		bytes.NewReader(raw), nil,
		gomp4.BoxPath{gomp4.BoxTypeMoof(), gomp4.BoxTypeMfhd()},
	)
	if err != nil {
		return 0, fmt.Errorf("go-mp4 extract mfhd: %w", err)
	}
	if len(boxes) == 0 {
		return 0, fmt.Errorf("go-mp4: mfhd not found")
	}
	mfhd, ok := boxes[0].Payload.(*gomp4.Mfhd)
	if !ok {
		return 0, fmt.Errorf("go-mp4: mfhd payload has type %T", boxes[0].Payload)
	}
	return mfhd.SequenceNumber, nil
}
