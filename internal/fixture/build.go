package fixture

import (
	"fmt"
	"sort"
)

// Build assembles a complete Matroska document.
func Build(tracks []TrackSpec, blocks []BlockSpec, layout Layout) Document {
	return buildWithBlocks(tracks, blocks, layout, func(spec BlockSpec, rel int64) []byte {
		return Block(spec, rel)
	})
}

// buildWithBlocks is the shared assembler; encode turns one block spec into
// its element bytes.
func buildWithBlocks(tracks []TrackSpec, blocks []BlockSpec, layout Layout, encode func(BlockSpec, int64) []byte) Document {
	if layout.DocType == "" {
		layout.DocType = "matroska"
	}
	if layout.TimestampScale == 0 {
		layout.TimestampScale = 1_000_000
	}
	if layout.ClusterSpan <= 0 {
		layout.ClusterSpan = 8
	}
	if layout.Duration == 0 {
		layout.Duration = 10_000
	}
	specs := append([]BlockSpec(nil), blocks...)
	if layout.SortByTime {
		sort.SliceStable(specs, func(i, j int) bool { return specs[i].Tick < specs[j].Tick })
	}
	clusters := groupClusters(specs, layout.ClusterSpan)

	doc := Document{}

	// Cues are computed from cluster offsets, so build the body first.
	body := NewEncoder()
	info := NewEncoder()
	info.UintElement(0x2AD7B1, layout.TimestampScale) // TimestampScale
	info.FloatElement(0x4489, layout.Duration)        // Duration
	info.StringElement(0x4D80, "archivecheck-fixture")
	info.StringElement(0x5741, "archivecheck-fixture")
	body.Element(0x1549A966, info.Bytes())

	tracksEnc := NewEncoder()
	for _, t := range tracks {
		te := NewEncoder()
		te.UintElement(0xD7, t.Number)
		te.UintElement(0x73C5, t.Number)
		te.UintElement(0x83, t.Type)
		te.StringElement(0x86, t.CodecID)
		if t.Language != "" {
			te.StringElement(0x22B59C, t.Language)
		}
		if t.DefaultDur > 0 {
			te.UintElement(0x23E383, t.DefaultDur)
		}
		if len(t.CodecPrivate) > 0 {
			te.Element(0x63A2, t.CodecPrivate)
		}
		switch t.Type {
		case 1:
			v := NewEncoder()
			v.UintElement(0xB0, t.Width)
			v.UintElement(0xBA, t.Height)
			te.Element(0xE0, v.Bytes())
		case 2:
			a := NewEncoder()
			a.FloatElement(0xB5, float64(t.SampleRate))
			a.UintElement(0x9F, t.Channels)
			te.Element(0xE1, a.Bytes())
		}
		tracksEnc.Element(0xAE, te.Bytes())
	}
	body.Element(0x1654AE6B, tracksEnc.Bytes())

	// Everything written before the Clusters counts toward their
	// Segment-relative position.
	prefixLen := body.Len()

	// Cues need each Cluster's Segment-relative offset, which depends on the
	// width of that Cluster's own size vint. Assemble the Cluster bytes first
	// with a placeholder Cues length, then rebuild once the real Cues length
	// is known. Both passes are deterministic, so the second is final.
	cuesLen := 0
	var clusterPlan clustersLayout
	for pass := 0; pass < 2; pass++ {
		clusterPlan = assembleClusters(clusters, layout, prefixLen, cuesLen, encode)
		if layout.NoCues {
			break
		}
		cuesBytes := buildCues(tracks, clusters, clusterPlan.offsets, cuesLen)
		if len(cuesBytes) == cuesLen {
			break
		}
		cuesLen = len(cuesBytes)
	}
	var cuesBytes []byte
	if !layout.NoCues {
		cuesBytes = buildCues(tracks, clusters, clusterPlan.offsets, cuesLen)
	}
	doc.CueCount = len(clusters)

	if !layout.NoCues && layout.CuesFirst {
		body.Raw(cuesBytes)
	}
	body.Raw(clusterPlan.bytes)
	if !layout.NoCues && !layout.CuesFirst {
		body.Raw(cuesBytes)
	}

	seg := NewEncoder()
	totalSize := body.Len()

	ebmlHdr := NewEncoder()
	ebmlHdr.UintElement(0x4286, 1)
	ebmlHdr.UintElement(0x42F7, 1)
	ebmlHdr.UintElement(0x42F2, 4)
	ebmlHdr.UintElement(0x42F3, 8)
	ebmlHdr.StringElement(0x4282, layout.DocType)
	ebmlHdr.UintElement(0x4287, 4)
	ebmlHdr.UintElement(0x4285, 2)
	seg.Element(0x1A45DFA3, ebmlHdr.Bytes())

	segmentHeaderStart := seg.Len()
	if layout.UnknownSegment {
		seg.HeaderUnknown(0x18538067)
	} else {
		seg.Header(0x18538067, totalSize)
	}
	segmentDataStart := seg.Len()
	doc.SegmentDataOffset = int64(segmentDataStart)
	seg.Raw(body.Bytes())

	doc.Bytes = seg.Bytes()
	doc.SegmentSizeOffset = segmentHeaderStart + len(EncID(0x18538067))
	doc.SegmentSizeWidth = 0
	if !layout.UnknownSegment {
		doc.SegmentSizeWidth = len(EncSize(uint64(totalSize)))
	}
	// Cluster offsets are already Segment-relative; add the Segment payload
	// start to obtain absolute file offsets.
	clusterBase := int64(segmentDataStart)
	runLeading := prefixLen
	if !layout.NoCues && layout.CuesFirst {
		runLeading += cuesLen
	}
	for _, off := range clusterPlan.offsets {
		doc.ClusterOffsets = append(doc.ClusterOffsets, clusterBase+off)
	}
	for _, cso := range clusterPlan.sizeFields {
		if cso[0] < 0 {
			doc.ClusterSizeOffsets = append(doc.ClusterSizeOffsets, [2]int{-1, 0})
			continue
		}
		doc.ClusterSizeOffsets = append(doc.ClusterSizeOffsets,
			[2]int{int(clusterBase) + runLeading + cso[0], cso[1]})
	}
	doc.CuesOffset = -1
	if !layout.NoCues {
		if layout.CuesFirst {
			doc.CuesOffset = clusterBase + int64(prefixLen)
		} else {
			doc.CuesOffset = clusterBase + int64(prefixLen) + int64(len(clusterPlan.bytes))
		}
	}
	return doc
}

func groupClusters(specs []BlockSpec, span int) [][]BlockSpec {
	var out [][]BlockSpec
	for i := 0; i < len(specs); i += span {
		end := i + span
		if end > len(specs) {
			end = len(specs)
		}
		out = append(out, specs[i:end])
	}
	return out
}

// BlockGroup encodes a BlockGroup with an optional Keyframe flag and
// BlockDuration, which SimpleBlock cannot carry.
func BlockGroup(spec BlockSpec, rel int64, duration int64) []byte {
	block := NewEncoder()
	block.Raw(EncTrackVint(spec.Track))
	block.Raw([]byte{byte(uint16(rel) >> 8), byte(rel)})
	flags := byte(0)
	if spec.Laced > 1 {
		flags |= 2 << 1 // fixed lacing
	}
	block.Raw([]byte{flags})
	if spec.Laced > 1 {
		if len(spec.Data)%spec.Laced != 0 {
			panic("fixture: laced payload must divide evenly")
		}
		block.Raw([]byte{byte(spec.Laced - 1)})
	}
	block.Raw(spec.Data)

	inner := NewEncoder()
	inner.Element(0xA1, block.Bytes()) // Block
	if spec.Key {
		inner.UintElement(0x9D, 1) // Keyframe
	} else {
		inner.IntElement(0xFB, -1) // ReferenceBlock
	}
	if duration > 0 {
		inner.IntElement(0x9B, duration) // BlockDuration
	}
	out := NewEncoder()
	out.Element(0xA0, inner.Bytes()) // BlockGroup
	return out.Bytes()
}

// GroupedBlockSpec attaches a BlockDuration to a block.
type GroupedBlockSpec struct {
	BlockSpec
	Duration int64
}

// BuildGrouped builds a document whose blocks are BlockGroups carrying
// explicit durations. Durations are matched to specs by (track, tick, payload)
// so identical specs stay distinguishable without extra IDs.
func BuildGrouped(tracks []TrackSpec, blocks []GroupedBlockSpec, layout Layout) Document {
	specs := make([]BlockSpec, len(blocks))
	durations := make(map[string]int64, len(blocks))
	for i, b := range blocks {
		specs[i] = b.BlockSpec
		durations[groupKey(b.BlockSpec)] = b.Duration
	}
	return buildWithBlocks(tracks, specs, layout, func(spec BlockSpec, rel int64) []byte {
		return BlockGroup(spec, rel, durations[groupKey(spec)])
	})
}

func groupKey(spec BlockSpec) string {
	return fmt.Sprintf("%d/%d/%x", spec.Track, spec.Tick, spec.Data)
}

// Block encodes one SimpleBlock element at the given relative timestamp.
func Block(spec BlockSpec, rel int64) []byte {
	block := NewEncoder()
	block.Raw(EncTrackVint(spec.Track))
	block.Raw([]byte{byte(uint16(rel) >> 8), byte(rel)})
	flags := byte(0)
	if spec.Key {
		flags |= 0x80
	}
	if spec.Laced > 1 {
		flags |= 2 << 1 // fixed lacing
	}
	block.Raw([]byte{flags})
	if spec.Laced > 1 {
		if len(spec.Data)%spec.Laced != 0 {
			panic("fixture: laced payload must divide evenly")
		}
		block.Raw([]byte{byte(spec.Laced - 1)})
	}
	block.Raw(spec.Data)
	out := NewEncoder()
	out.Element(0xA3, block.Bytes())
	return out.Bytes()
}

// EncTrackVint encodes a track number as an EBML vint.
func EncTrackVint(track uint64) []byte {
	return EncSize(track)
}

// clustersLayout is one assembled Cluster run.
type clustersLayout struct {
	bytes []byte
	// offsets are byte offsets of each Cluster within the run.
	offsets []int64
	// sizeFields locates each known-size Cluster's size vint within the run.
	sizeFields [][2]int
}

// assembleClusters builds the Cluster bytes with Segment-relative offsets
// computed as if the Cues payload occupied cuesLen bytes before them.
func assembleClusters(clusters [][]BlockSpec, layout Layout, prefixLen, cuesLen int, encode func(BlockSpec, int64) []byte) clustersLayout {
	// Offsets are measured from the start of the Segment payload, so the
	// Clusters must account for whatever precedes them.
	leading := prefixLen
	if !layout.NoCues && layout.CuesFirst {
		leading += cuesLen
	}
	var out clustersLayout
	for _, cl := range clusters {
		ce := NewEncoder()
		ts := cl[0].Tick
		ce.UintElement(0xE7, uint64(ts))
		for _, b := range cl {
			ce.Raw(encode(b, b.Tick-ts))
		}
		start := int64(leading + len(out.bytes))
		out.offsets = append(out.offsets, start)
		idLen := len(EncID(0x1F43B675))
		if layout.UnknownCluster {
			out.bytes = append(out.bytes, EncID(0x1F43B675)...)
			out.bytes = append(out.bytes, UnknownSize...)
			out.sizeFields = append(out.sizeFields, [2]int{-1, 0})
		} else {
			sizeWidth := len(EncSize(uint64(len(ce.Bytes()))))
			out.sizeFields = append(out.sizeFields, [2]int{len(out.bytes) + idLen, sizeWidth})
			out.bytes = append(out.bytes, EncID(0x1F43B675)...)
			out.bytes = append(out.bytes, EncSize(uint64(len(ce.Bytes())))...)
		}
		out.bytes = append(out.bytes, ce.Bytes()...)
	}
	return out
}

func buildCues(tracks []TrackSpec, clusters [][]BlockSpec, offsets []int64, cuesLen int) []byte {
	cues := NewEncoder()
	for i, cl := range clusters {
		if len(cl) == 0 {
			continue
		}
		// Emit one cue point per track for the first keyframe in the cluster.
		seen := map[uint64]bool{}
		for j, b := range cl {
			if !b.Key || seen[b.Track] {
				continue
			}
			seen[b.Track] = true
			cp := NewEncoder()
			cp.UintElement(0xB3, uint64(b.Tick))
			ctp := NewEncoder()
			ctp.UintElement(0xF7, b.Track)
			ctp.UintElement(0xF1, uint64(offsets[i]))
			ctp.UintElement(0x5378, uint64(j+1))
			cp.Element(0xB7, ctp.Bytes())
			cues.Element(0xBB, cp.Bytes())
		}
	}
	out := NewEncoder()
	out.Element(0x1C53BB6B, cues.Bytes())
	return out.Bytes()
}

// DefaultTracks returns a video, an audio and a subtitle track.
func DefaultTracks() []TrackSpec {
	return []TrackSpec{
		{Number: 1, Type: 1, CodecID: "V_VP9", Width: 320, Height: 180, DefaultDur: 33_333_333},
		{Number: 2, Type: 2, CodecID: "A_OPUS", SampleRate: 48000, Channels: 2, DefaultDur: 20_000_000},
		{Number: 3, Type: 17, CodecID: "S_TEXT/UTF8", DefaultDur: 1_000_000_000},
	}
}

// DefaultBlocks returns a deterministic set of interleaved blocks.
func DefaultBlocks() []BlockSpec {
	var out []BlockSpec
	for i := 0; i < 12; i++ {
		tick := int64(i * 40)
		out = append(out,
			BlockSpec{Track: 1, Tick: tick, Key: i%4 == 0, Data: Pattern("video", i, 96)},
			BlockSpec{Track: 2, Tick: tick, Key: true, Data: Pattern("audio", i, 64)},
		)
		if i%4 == 0 {
			out = append(out, BlockSpec{Track: 3, Tick: tick, Key: true, Data: []byte(fmt.Sprintf("subtitle %d", i))})
		}
	}
	return out
}

// Pattern generates deterministic, distinct sample bytes.
func Pattern(kind string, i, n int) []byte {
	out := make([]byte, n)
	seed := byte(i*31 + len(kind))
	for j := range out {
		out[j] = seed + byte(j*7) + byte(kind[j%len(kind)])
	}
	return out
}
