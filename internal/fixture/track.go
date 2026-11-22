package fixture

import (
	"encoding/binary"
)

// buildTrack assembles a trak box from generated sample payloads.
func (g *mp4Gen) buildTrack(id uint32, handler, entry string, timescale uint32,
	payload [][]byte, keyframeEvery int, chunkOffset int64, editMediaTime int64,
	emptyEdit bool, video bool) []byte {

	n := len(payload)
	var delta uint32
	if timescale > 0 {
		delta = g.unitDelta(n, timescale)
	}
	duration := uint32(uint64(delta) * uint64(n))

	tkhdBox := tkhd(id, duration, 1920, 1080)
	mdhdBox := g.mdhd(timescale, duration)
	hdlrBox := hdlr(handler)

	stsdBox := g.stsd(entry, video)
	sttsBox := stts(n, delta)
	stscBox := stsc(1, uint32(n), 1)
	stszBox := stsz(payload)
	var cttsBox []byte
	if video {
		cttsBox = ctts(n)
	}
	var stssBox []byte
	if g.opts.Corrupt == CorruptBadStss && video {
		stssBox = stss([]uint32{1, uint32(n) + 500})
	} else if keyframeEvery > 0 {
		var nums []uint32
		for i := 0; i < n; i += keyframeEvery {
			nums = append(nums, uint32(i+1))
		}
		stssBox = stss(nums)
	}

	stcoOffset := chunkOffset
	if g.opts.Corrupt == CorruptBadChunkOffset && video {
		stcoOffset = chunkOffset + 1<<30
	}
	stcoBox := stco(stcoOffset)

	stblParts := [][]byte{stsdBox, sttsBox}
	if cttsBox != nil {
		stblParts = append(stblParts, cttsBox)
	}
	stblParts = append(stblParts, stscBox, stszBox, stcoBox)
	if stssBox != nil {
		stblParts = append(stblParts, stssBox)
	}
	stbl := mp4Box("stbl", concat(stblParts...))

	var mediaHeader []byte
	if video {
		mediaHeader = fullBox("vmhd", 0, 1, concat(u16(0), u16(0), u16(0), u16(0)))
	} else if handler == "soun" {
		mediaHeader = fullBox("smhd", 0, 0, concat(u16(0), u16(0)))
	} else {
		mediaHeader = fullBox("nmhd", 0, 0, nil)
	}
	minf := mp4Box("minf", concat(mediaHeader, dinf(), stbl))
	mdia := mp4Box("mdia", concat(mdhdBox, hdlrBox, minf))

	var edts []byte
	switch {
	case g.opts.Corrupt == CorruptEmptyEdit && video:
		// A well-formed elst whose entry_count is zero: no timeline mapping.
		edts = mp4Box("edts", elst(nil))
	case g.opts.Corrupt == CorruptNegativeEdit && video:
		// A media_time below -1 is meaningless and must be rejected.
		edts = mp4Box("edts", elst([][2]int64{{int64(duration), editMediaTime}}))
	case g.opts.Corrupt == CorruptNone && (editMediaTime != 0 || emptyEdit):
		var entries [][2]int64
		if emptyEdit {
			entries = append(entries, [2]int64{1000, -1})
		}
		if editMediaTime >= 0 {
			entries = append(entries, [2]int64{int64(duration), editMediaTime})
		}
		edts = mp4Box("edts", elst(entries))
	}

	parts := [][]byte{tkhdBox}
	if edts != nil {
		parts = append(parts, edts)
	}
	parts = append(parts, mdia)
	return mp4Box("trak", concat(parts...))
}

// dinf builds a minimal data-information box with a self-contained url.
func dinf() []byte {
	url := fullBox("url ", 0, 1, nil) // flags=1 → media data in same file
	dref := fullBox("dref", 0, 0, concat(u32(1), url))
	return mp4Box("dinf", dref)
}

// stsd builds a sample description with a single synthetic entry. The entry
// payload is opaque to the remux kernel (it is copied verbatim) but carries
// width/height (video) and channel/rate fields (audio) for provenance.
func (g *mp4Gen) stsd(entry string, video bool) []byte {
	var sampleEntry []byte
	switch entry {
	case "avc1":
		// sample entry header (6 reserved + 2 dri) then visual fields.
		body := concat(
			make([]byte, 6), u16(1), // reserved, data_reference_index
			u16(0), u16(0), u32(0), u32(0), u32(0), u16(1920), u16(1080),
			u32(0x00480000), u32(0x00480000), u32(0), u16(1),
			make([]byte, 32), // compressor name
			u16(0x0018), u16(0xffff),
			mp4Box("avcC", concat([]byte{1, 0x64, 0, 0x1f, 0xff, 0xe1}, []byte{0, 4, 0x67, 0x64, 0, 0x1f}, []byte{1, 0, 4, 0x68, 0xee, 0x3c, 0x80})),
		)
		sampleEntry = mp4Box("avc1", body)
	case "mp4a":
		body := concat(
			make([]byte, 6), u16(1),
			u32(0), u32(0),
			u16(2), u16(16), u16(0), u16(0),
			u32(48000<<16),
			mp4Box("esds", concat([]byte{0, 0, 0, 0}, []byte{0x03, 0x19, 0x00, 0x00, 0x00, 0x04, 0x11, 0x40, 0x15, 0x00, 0x00, 0x00, 0x00, 0x01, 0xf4, 0x00, 0x00, 0x01, 0xf4, 0x00, 0x05, 0x02, 0x12, 0x10})),
		)
		sampleEntry = mp4Box("mp4a", body)
	case "tmcd":
		body := concat(
			make([]byte, 6), u16(1),
			u32(0), u32(0), u32(0), u32(0),
			u32(600<<16), u16(0),
			u32(0), u32(1),
		)
		sampleEntry = mp4Box("tmcd", body)
	default:
		sampleEntry = mp4Box(entry, concat(make([]byte, 6), u16(1)))
	}
	return fullBox("stsd", 0, 0, concat(u32(1), sampleEntry))
}

// corruptStss rewrites the video stss to reference an out-of-range sample.
func (g *mp4Gen) corruptStss(video []byte) []byte {
	// Rebuild with a deliberately bad sync sample number.
	return g.rebuildVideoStss(video, []uint32{1, 999999})
}

// rebuildVideoStss is a thin wrapper that replaces the stss inside a trak. It
// is only used by fixture generation and does not need to be fast.
func (g *mp4Gen) rebuildVideoStss(video []byte, nums []uint32) []byte {
	// The fixture generator knows the exact layout: find the "stss" box and
	// replace its payload in place.
	idx := indexOf(video, []byte("stss"))
	if idx < 0 {
		return video
	}
	boxStart := idx - 4
	oldSize := int(binary.BigEndian.Uint32(video[boxStart:]))
	newBox := stss(nums)
	out := make([]byte, 0, len(video)-oldSize+len(newBox))
	out = append(out, video[:boxStart]...)
	out = append(out, newBox...)
	out = append(out, video[boxStart+oldSize:]...)
	return out
}

// patchChunkOffset rewrites the single stco offset inside a trak.
func (g *mp4Gen) patchChunkOffset(track []byte, newOffset int64) []byte {
	idx := indexOf(track, []byte("stco"))
	if idx < 0 {
		return track
	}
	// stco payload: version/flags (4) + entry_count (4) + offset (4).
	offPos := idx + 4 + 4 + 4
	if offPos+4 > len(track) {
		return track
	}
	binary.BigEndian.PutUint32(track[offPos:], uint32(newOffset))
	return track
}

func indexOf(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
