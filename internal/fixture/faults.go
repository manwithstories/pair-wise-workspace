package fixture

import (
	"encoding/binary"
	"math"

	"github.com/pairwise/relay/internal/cache"
	"github.com/pairwise/relay/internal/isobmff"
	"github.com/pairwise/relay/internal/media"
	"github.com/pairwise/relay/internal/pipeline"
)

func damageSamples() []pipeline.DamageSample {
	samples := []isobmff.SampleConfig{{Duration: 30000, Size: 32}, {Duration: 30000, Size: 40}}
	valid, err := isobmff.BuildFragment(isobmff.FragmentConfig{Sequence: 99, TFDT: 99_000, Samples: samples})
	if err != nil {
		panic(err)
	}
	cut := len(valid) - 4
	truncated := append([]byte(nil), valid[:cut]...)

	badLength := []byte{0, 0, 0, 4, 'b', 'a', 'd', '!'}

	overflow := make([]byte, 16)
	binary.BigEndian.PutUint32(overflow[0:4], 1)
	copy(overflow[4:8], "ovf!")
	binary.BigEndian.PutUint64(overflow[8:16], math.MaxUint64)

	return []pipeline.DamageSample{
		{
			Name: "truncated-mdat", Data: truncated, Rendition: "damage", Segment: 99,
			WantCode: string(media.CodeBoxTruncated), WantOffset: int64(cut),
		},
		{
			Name: "box-length-too-small", Data: badLength, Rendition: "damage", Segment: 100,
			WantCode: string(media.CodeBoxLength), WantOffset: 0,
		},
		{
			Name: "extended-size-overflow", Data: overflow, Rendition: "damage", Segment: 101,
			WantCode: string(media.CodeIntegerOverflow), WantOffset: 0,
		},
	}
}

func forgedCacheEntry(streamID string) pipeline.ForgeCacheEntry {
	return pipeline.ForgeCacheEntry{
		Key: cache.Key{
			SourceNode: "edge-forged",
			StreamID:   streamID,
			Rendition:  "high",
			Segment:    1,
		},
		Data: []byte("forged cache origin sample; sha will not match reprocessed fragment"),
	}
}
