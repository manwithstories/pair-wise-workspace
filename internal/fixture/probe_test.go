package fixture

import "testing"

func TestProbeClusterOffsets(t *testing.T) {
	doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, ClusterSpan: 6})
	t.Logf("len=%d segDataOffset=%d segSizeOffset=%d width=%d", len(doc.Bytes), doc.SegmentDataOffset, doc.SegmentSizeOffset, doc.SegmentSizeWidth)
	t.Logf("clusterOffsets=%v", doc.ClusterOffsets)
	bad, err := New(DamageOversized, 17)
	t.Logf("err=%v note=%s", err, bad.Note)
	t.Logf("seg size bytes=% x", bad.Bytes[doc.SegmentSizeOffset:doc.SegmentSizeOffset+doc.SegmentSizeWidth])
}
