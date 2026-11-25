package fixture

import (
	"encoding/binary"

	"avlab/internal/bmff"
	"avlab/internal/media"
)

// DamagedCase is one malformed input and the Fault code it must produce.
type DamagedCase struct {
	Name string
	Data []byte
	Code media.Code
}

// BuildDamagedCases mutates a valid segment into four independent damaged
// variants. Each variant stays self-contained: no external files needed.
func BuildDamagedCases(valid []byte) []DamagedCase {
	outOfBounds := append([]byte(nil), valid...)
	binary.BigEndian.PutUint32(outOfBounds[0:4],
		uint32(len(outOfBounds))+100)

	truncated := append([]byte(nil), valid[:len(valid)/2]...)

	overflowSpec := bmff.FragmentSpec{
		Sequence: 1, TrackID: 1, BaseTime: media.MaxTick,
		Samples: []bmff.SampleDesc{
			{Size: 4, Duration: 3000, Keyframe: true, Payload: []byte("abcd")},
		},
	}
	overflowData, _ := bmff.BuildFragment(overflowSpec)

	// mdat undersized: inflate the first trun sample's declared size while
	// every box boundary stays intact.
	mdatSmall := append([]byte(nil), valid...)
	if pos := locateTrun(mdatSmall); pos >= 0 {
		sampleSizePos := pos + 8 + 4 + 4 + 4 + 4
		binary.BigEndian.PutUint32(mdatSmall[sampleSizePos:], 1_000_000)
	}

	return []DamagedCase{
		{"box_out_of_bounds", outOfBounds, media.CodeBoxOutOfBounds},
		{"box_truncated", truncated, media.CodeBoxOutOfBounds},
		{"timestamp_overflow", overflowData, media.CodeTimestampOverflow},
		{"mdat_undersized", mdatSmall, media.CodeBoxOutOfBounds},
	}
}

// locateTrun walks top-level moof children and returns the trun box offset.
func locateTrun(data []byte) int {
	for p := 0; p+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[p:]))
		if size <= 0 || p+size > len(data) {
			return -1
		}
		if string(data[p+4:p+8]) == "moof" {
			q := p + 8
			for q+8 < p+size {
				inner := int(binary.BigEndian.Uint32(data[q:]))
				if inner <= 0 {
					return -1
				}
				if string(data[q+4:q+8]) == "trun" {
					return q
				}
				q += inner
			}
		}
		p += size
	}
	return -1
}
