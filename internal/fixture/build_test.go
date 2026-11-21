package fixture

import (
	"bytes"
	"fmt"
	"testing"

	"archivecheck.local/archivecheck/internal/demux"
)

func scanDoc(t *testing.T, doc Document, opts demux.Options) (*demux.Result, []demux.Sample) {
	t.Helper()
	res := &demux.Result{}
	var samples []demux.Sample
	opts.OnSample = func(s demux.Sample) error {
		cp := s
		cp.Data = append([]byte(nil), s.Data...)
		samples = append(samples, cp)
		return nil
	}
	r := bytes.NewReader(doc.Bytes)
	if err := demux.ScanReader(r, opts, res); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return res, samples
}

func TestBuildScansCleanly(t *testing.T) {
	doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, ClusterSpan: 5})
	res, samples := scanDoc(t, doc, demux.DefaultOptions())
	if res.Info.DocType != "matroska" {
		t.Fatalf("doctype=%q", res.Info.DocType)
	}
	if res.Info.TimestampScale != 1_000_000 {
		t.Fatalf("scale=%d", res.Info.TimestampScale)
	}
	if len(res.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(res.Tracks))
	}
	if len(samples) != 12*2+3 {
		t.Fatalf("samples=%d", len(samples))
	}
	if !res.Info.HasCues || res.Info.CueEntries == 0 {
		t.Fatalf("cues not found: %+v", res.Info)
	}
	for _, s := range samples {
		if s.SourceOffset < 0 || s.SourceOffset+int64(len(s.Data)) > int64(len(doc.Bytes)) {
			t.Fatalf("sample offset outside file: %+v", s)
		}
		if !bytes.Equal(doc.Bytes[s.SourceOffset:s.SourceOffset+int64(len(s.Data))], s.Data) {
			t.Fatalf("sample data is not a substring of the source")
		}
	}
}

func TestBuildUnknownSegmentAndCluster(t *testing.T) {
	doc := Build(DefaultTracks(), DefaultBlocks(), Layout{
		SortByTime: true, ClusterSpan: 4, UnknownSegment: true, UnknownCluster: true,
	})
	res, samples := scanDoc(t, doc, demux.DefaultOptions())
	if !res.Info.SegmentUnknown {
		t.Fatal("segment should be unknown-size")
	}
	if len(samples) != 27 {
		t.Fatalf("samples=%d", len(samples))
	}
}

func TestBuildNoCues(t *testing.T) {
	doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, NoCues: true})
	res, _ := scanDoc(t, doc, demux.DefaultOptions())
	if res.Info.HasCues || res.Info.CueEntries != 0 {
		t.Fatalf("cues should be absent: %+v", res.Info)
	}
}

func TestBuildOutOfOrderBlocks(t *testing.T) {
	blocks := []BlockSpec{
		{Track: 1, Tick: 80, Key: true, Data: Pattern("v", 8, 16)},
		{Track: 1, Tick: 0, Key: true, Data: Pattern("v", 0, 16)},
		{Track: 1, Tick: 40, Key: false, Data: Pattern("v", 4, 16)},
	}
	doc := Build(DefaultTracks()[:1], blocks, Layout{ClusterSpan: 8})
	res, samples := scanDoc(t, doc, demux.DefaultOptions())
	if len(samples) != 3 {
		t.Fatalf("samples=%d", len(samples))
	}
	if samples[0].Timestamp != 80 || samples[1].Timestamp != 0 {
		t.Fatalf("source order not preserved: %d,%d", samples[0].Timestamp, samples[1].Timestamp)
	}
	_ = res
}

func TestUnknownTrackBlocksCounted(t *testing.T) {
	blocks := []BlockSpec{
		{Track: 1, Tick: 0, Key: true, Data: Pattern("v", 0, 16)},
		{Track: 99, Tick: 0, Key: true, Data: Pattern("x", 0, 16)},
	}
	doc := Build(DefaultTracks()[:1], blocks, Layout{ClusterSpan: 8})
	res, samples := scanDoc(t, doc, demux.DefaultOptions())
	if len(samples) != 1 {
		t.Fatalf("declared samples=%d", len(samples))
	}
	if res.UnknownTrackBlocks != 1 || len(res.UnknownTrackNumbers) != 1 || res.UnknownTrackNumbers[0] != 99 {
		t.Fatalf("unknown stats: %+v", res)
	}
}

func TestLacedAudioSplitsIntoSamples(t *testing.T) {
	blocks := []BlockSpec{
		{Track: 1, Tick: 0, Key: true, Data: Pattern("v", 0, 32)},
		{Track: 2, Tick: 0, Key: true, Laced: 3, Data: Pattern("a", 0, 48)},
	}
	doc := Build(DefaultTracks()[:2], blocks, Layout{ClusterSpan: 8})
	_, samples := scanDoc(t, doc, demux.DefaultOptions())
	if len(samples) != 4 {
		t.Fatalf("samples=%d", len(samples))
	}
	if samples[2].FrameIndex != 1 || !samples[2].Laced {
		t.Fatalf("lacing not reported: %+v", samples[2])
	}
}

func TestClusterAndSizeFieldOffsets(t *testing.T) {
	doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, ClusterSpan: 6})
	if len(doc.ClusterOffsets) == 0 || len(doc.ClusterSizeOffsets) != len(doc.ClusterOffsets) {
		t.Fatalf("clusters=%d sizeFields=%d", len(doc.ClusterOffsets), len(doc.ClusterSizeOffsets))
	}
	for i, off := range doc.ClusterOffsets {
		if off <= 0 || int(off) >= len(doc.Bytes) {
			t.Fatalf("cluster %d offset %d outside file", i, off)
		}
		if !bytes.HasPrefix(doc.Bytes[off:], EncID(0x1F43B675)) {
			t.Fatalf("cluster %d does not start with the Cluster ID", i)
		}
		cso := doc.ClusterSizeOffsets[i]
		if cso[0] < 0 {
			t.Fatalf("cluster %d should have a known size", i)
		}
		// The size vint must live immediately after the 4-byte Cluster ID.
		if cso[0] != int(off)+len(EncID(0x1F43B675)) {
			t.Fatalf("cluster %d size field at %d, ID at %d", i, cso[0], off)
		}
	}
	if doc.CuesOffset <= 0 || !bytes.HasPrefix(doc.Bytes[doc.CuesOffset:], EncID(0x1C53BB6B)) {
		t.Fatalf("cues offset %d is not a Cues element", doc.CuesOffset)
	}
}

func TestKnownCueOffsetsPointAtClusters(t *testing.T) {
	doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, ClusterSpan: 6})
	payload := cuePayload(t, doc)
	points, err := parseCuePoints(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) == 0 {
		t.Fatal("no cue points")
	}
	inCluster := map[int64]bool{}
	for _, off := range doc.ClusterOffsets {
		inCluster[off-int64(doc.SegmentDataOffset)] = true
	}
	for _, p := range points {
		if !inCluster[p] {
			t.Fatalf("cue position %d does not name a Cluster", p)
		}
	}
}

// cuePayload extracts the Cues payload from a built document.
func cuePayload(t *testing.T, doc Document) []byte {
	t.Helper()
	if doc.CuesOffset < 0 {
		t.Fatal("document has no Cues")
	}
	start := int(doc.CuesOffset) + len(EncID(0x1C53BB6B))
	w := 1
	for b := doc.Bytes[start]; b&0x80 == 0; b <<= 1 {
		w++
	}
	var v uint64
	for i := 0; i < w; i++ {
		v = v<<8 | uint64(doc.Bytes[start+i])
	}
	v &^= uint64(1) << (7 * uint(w))
	return doc.Bytes[start+w : start+w+int(v)]
}

// parseCuePoints returns the ClusterPosition of every CuePoint.
func parseCuePoints(payload []byte) ([]int64, error) {
	var out []int64
	i := 0
	for i < len(payload) {
		id, n := readID(payload[i:])
		if n == 0 {
			return nil, fmt.Errorf("bad cue ID at %d", i)
		}
		i += n
		size, n := readSize(payload[i:])
		if n == 0 {
			return nil, fmt.Errorf("bad cue size at %d", i)
		}
		i += n
		if id == 0xBB {
			pos, err := findClusterPosition(payload[i : i+int(size)])
			if err != nil {
				return nil, err
			}
			out = append(out, pos)
		}
		i += int(size)
	}
	return out, nil
}

func findClusterPosition(cuePoint []byte) (int64, error) {
	i := 0
	for i < len(cuePoint) {
		id, n := readID(cuePoint[i:])
		if n == 0 {
			return 0, fmt.Errorf("bad ID")
		}
		i += n
		size, n := readSize(cuePoint[i:])
		if n == 0 {
			return 0, fmt.Errorf("bad size")
		}
		i += n
		if id == 0xB7 {
			inner := cuePoint[i : i+int(size)]
			j := 0
			for j < len(inner) {
				cid, cn := readID(inner[j:])
				if cn == 0 {
					return 0, fmt.Errorf("bad child ID")
				}
				j += cn
				csz, sn := readSize(inner[j:])
				if sn == 0 {
					return 0, fmt.Errorf("bad child size")
				}
				j += sn
				if cid == 0xF1 {
					var v uint64
					for _, b := range inner[j : j+int(csz)] {
						v = v<<8 | uint64(b)
					}
					return int64(v), nil
				}
				j += int(csz)
			}
		}
		i += int(size)
	}
	return 0, fmt.Errorf("no ClusterPosition")
}

func readID(b []byte) (uint32, int) {
	if len(b) == 0 {
		return 0, 0
	}
	var n int
	switch {
	case b[0]&0x80 != 0:
		n = 1
	case b[0]&0x40 != 0:
		n = 2
	case b[0]&0x20 != 0:
		n = 3
	case b[0]&0x10 != 0:
		n = 4
	default:
		return 0, 0
	}
	if len(b) < n {
		return 0, 0
	}
	var id uint32
	for i := 0; i < n; i++ {
		id = id<<8 | uint32(b[i])
	}
	return id, n
}

func readSize(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	n := 1
	for v := b[0]; v&0x80 == 0; v <<= 1 {
		n++
	}
	if len(b) < n {
		return 0, 0
	}
	var size uint64
	for i := 0; i < n; i++ {
		size = size<<8 | uint64(b[i])
	}
	size &^= uint64(1) << (7 * uint(n))
	return size, n
}

func TestBuildGroupedCarriesDurations(t *testing.T) {
	tracks := []TrackSpec{{Number: 1, Type: 1, CodecID: "V_VP9", Width: 64, Height: 64}}
	blocks := []GroupedBlockSpec{
		{BlockSpec: BlockSpec{Track: 1, Tick: 0, Key: true, Data: Pattern("v", 0, 16)}, Duration: 30},
		{BlockSpec: BlockSpec{Track: 1, Tick: 40, Key: false, Data: Pattern("v", 1, 16)}, Duration: 30},
	}
	doc := BuildGrouped(tracks, blocks, Layout{ClusterSpan: 4})
	_, samples := scanDoc(t, doc, demux.DefaultOptions())
	if len(samples) != 2 {
		t.Fatalf("samples=%d", len(samples))
	}
	if samples[0].DurationTicks != 30 || samples[1].DurationTicks != 30 {
		t.Fatalf("sample durations=%d,%d", samples[0].DurationTicks, samples[1].DurationTicks)
	}
}
