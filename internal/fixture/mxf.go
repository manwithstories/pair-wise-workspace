package fixture

import (
	"bytes"
	"fmt"

	"relay.local/relay/internal/model"
)

// MXF universal-label prefixes for the relay profile.
var (
	ulHeaderPartition = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x04, 0x00}
	ulBodyPartition   = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x03, 0x00}
	ulFooterPartition = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x05, 0x00}
	ulPrimerPack      = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x05, 0x01, 0x00}
	ulTrackSet        = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x7f, 0x01, 0x00}
	ulIndexTable      = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x10, 0x01, 0x00}
	ulPictureEssence  = []byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01, 0x0d, 0x01, 0x03, 0x01, 0x15, 0x01, 0x05, 0x00}
)

// MXFTrackSpec describes one track to write into an MXF file.
type MXFTrackSpec struct {
	ID        uint32
	Kind      model.TrackKind
	Codec     model.Codec
	EditNum   uint32
	EditDen   uint32
	Duration  uint64
	Container map[uint16]string // container description claims by relay tag
	Bitstream map[uint16]string // bitstream description claims by relay tag
	Frames    [][]byte
	// Durations optionally overrides the index durations per frame.
	Durations []uint32
	// Keys optionally overrides the index key flags per frame; nil means
	// every frame is a keyframe.
	Keys []bool
	// CorruptIndex, when set, rewrites one index stream offset to a value
	// that points at no essence of the track.
	CorruptIndex int
}

// MXFSpec is a complete MXF file to write.
type MXFSpec struct {
	Tracks []MXFTrackSpec
}

// localItem encodes one tag-length-value item of a local set.
func localItem(tag uint16, value []byte) []byte {
	return cat(u16(tag), berLen(len(value)), value)
}

func localSet(parts ...[]byte) []byte {
	return cat(parts...)
}

// BuildMXF writes an MXF file with a header partition, primer, track sets,
// index tables and one picture essence KLV per frame.
func BuildMXF(spec MXFSpec) ([]byte, error) {
	if len(spec.Tracks) == 0 {
		return nil, fmt.Errorf("fixture: mxf needs at least one track")
	}
	var buf bytes.Buffer
	buf.Write(klv(ulHeaderPartition, partitionPack(0x04)))
	buf.Write(klv(ulPrimerPack, primerValue(spec)))

	for _, tr := range spec.Tracks {
		buf.Write(klv(ulTrackSet, trackSetValue(tr)))
	}
	buf.Write(klv(ulBodyPartition, partitionPack(0x03)))

	// Essence values start after the body partition, so index stream offsets
	// must be computed relative to the file position of each frame value.
	// We emit frames first, recording their absolute value offsets, then write
	// the index tables; MXF permits index segments to trail the essence.
	base := int64(buf.Len())
	valueOffsets := make([][]uint32, len(spec.Tracks))
	pos := base
	for ti, tr := range spec.Tracks {
		valueOffsets[ti] = make([]uint32, len(tr.Frames))
		for fi, frame := range tr.Frames {
			item := cat(u32(tr.ID), frame)
			klvBytes := klv(ulPictureEssence, item)
			// Stream offsets are absolute offsets of the frame payload: the
			// KLV key, the BER length and the 4-byte track number precede it.
			valueOffsets[ti][fi] = uint32(pos) + 16 + uint32(len(berLen(len(item)))) + 4
			buf.Write(klvBytes)
			pos += int64(len(klvBytes))
		}
	}
	for ti, tr := range spec.Tracks {
		buf.Write(klv(ulIndexTable, indexValue(tr, valueOffsets[ti])))
	}
	buf.Write(klv(ulFooterPartition, partitionPack(0x05)))
	return buf.Bytes(), nil
}

func partitionPack(position byte) []byte {
	// A compact but well-formed partition pack: major/minor version, KAG
	// size, this partition offset and body/footer offsets are not interpreted
	// by the parser beyond the 4-byte minimum, so zeros are valid here.
	key := make([]byte, 16)
	copy(key, ulHeaderPartition)
	key[14] = position
	return make([]byte, 88)
}

func primerValue(spec MXFSpec) []byte {
	var local bytes.Buffer
	local.Write(u16(0))
	n := 0
	add := func(tag uint16, name string) {
		local.Write(u16(tag))
		local.Write(berLen(len(name)))
		local.WriteString(name)
		n++
	}
	add(0x8001, "relay:TrackID")
	add(0x8002, "relay:Kind")
	add(0x8003, "relay:Codec")
	add(0x8004, "relay:EditRateNum")
	add(0x8005, "relay:EditRateDen")
	add(0x8006, "relay:Duration")
	add(0x3f01, "relay:IndexStart")
	add(0x3f02, "relay:IndexCount")
	add(0x3f03, "relay:IndexEntries")
	add(0x8007, "relay:ContainerRotation")
	add(0x8008, "relay:BitstreamRotation")
	add(0x8009, "relay:ContainerCrop")
	add(0x800a, "relay:BitstreamCrop")
	add(0x800b, "relay:ContainerPrimaries")
	add(0x800c, "relay:ContainerTransfer")
	add(0x800d, "relay:ContainerMatrix")
	add(0x800e, "relay:ContainerRange")
	add(0x800f, "relay:BitstreamPrimaries")
	add(0x8010, "relay:BitstreamTransfer")
	add(0x8011, "relay:BitstreamMatrix")
	add(0x8012, "relay:BitstreamRange")
	add(0x8013, "relay:ContainerColorMarker")
	add(0x8014, "relay:BitstreamColorMarker")
	add(0x8015, "relay:ContainerWidth")
	add(0x8016, "relay:ContainerHeight")
	add(0x8017, "relay:BitstreamWidth")
	add(0x8018, "relay:BitstreamHeight")
	add(0x8019, "relay:BitstreamAperture")
	return local.Bytes()
}

func claimValue(tag uint16, v string) []byte {
	switch tag {
	case 0x8007, 0x8008:
		var n int16
		fmt.Sscanf(v, "%d", &n)
		return u16(uint16(n))
	case 0x8009, 0x800a:
		var l, t, r, b int
		fmt.Sscanf(v, "%d,%d,%d,%d", &l, &t, &r, &b)
		return cat(u16(uint16(l)), u16(uint16(t)), u16(uint16(r)), u16(uint16(b)))
	case 0x800b, 0x800c, 0x800d, 0x800f, 0x8010, 0x8011:
		var n uint16
		fmt.Sscanf(v, "%d", &n)
		return u16(n)
	case 0x8015, 0x8016, 0x8017, 0x8018:
		var n uint32
		fmt.Sscanf(v, "%d", &n)
		return u32(n)
	case 0x8019:
		var w, h uint32
		fmt.Sscanf(v, "%dx%d", &w, &h)
		return cat(u32(w), u32(h))
	default:
		return []byte(v)
	}
}

func trackSetValue(tr MXFTrackSpec) []byte {
	editNum := tr.EditNum
	if editNum == 0 {
		editNum = MXFEditRate
	}
	editDen := tr.EditDen
	if editDen == 0 {
		editDen = 1
	}
	kind := tr.Kind
	if kind == "" {
		kind = model.Video
	}
	codec := tr.Codec
	if codec == "" {
		codec = model.CodecMJPEG
	}
	var local bytes.Buffer
	local.Write(localItem(0x8001, u32(tr.ID)))
	local.Write(localItem(0x8002, []byte(kind)))
	local.Write(localItem(0x8003, []byte(codec)))
	local.Write(localItem(0x8004, u32(editNum)))
	local.Write(localItem(0x8005, u32(editDen)))
	local.Write(localItem(0x8006, u64(tr.Duration)))
	local.Write(localItem(0x3f01, u64(0)))
	for _, tag := range sortedTags(tr.Container) {
		local.Write(localItem(tag, claimValue(tag, tr.Container[tag])))
	}
	for _, tag := range sortedTags(tr.Bitstream) {
		local.Write(localItem(tag, claimValue(tag, tr.Bitstream[tag])))
	}
	return localSet(local.Bytes())
}

func sortedTags(m map[uint16]string) []uint16 {
	out := make([]uint16, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion sort keeps a local copy; maps have at most 16 entries here.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func indexValue(tr MXFTrackSpec, offsets []uint32) []byte {
	var entries bytes.Buffer
	for i := 0; i < len(tr.Frames); i++ {
		stream := offsets[i]
		if tr.CorruptIndex == i+1 {
			stream += 0x7ffffff0
		}
		dur := uint32(1)
		if i < len(tr.Durations) {
			dur = tr.Durations[i]
		}
		key := uint32(1)
		if i < len(tr.Keys) && !tr.Keys[i] {
			key = 0
		}
		entries.Write(u32(stream))
		entries.Write(u32(dur))
		entries.Write(u32(uint32(i)))
		entries.Write(u32(key))
	}
	var local bytes.Buffer
	local.Write(localItem(0x8001, u32(tr.ID)))
	local.Write(localItem(0x3f01, u64(0)))
	local.Write(localItem(0x3f02, u32(uint32(len(tr.Frames)))))
	local.Write(localItem(0x3f03, entries.Bytes()))
	return local.Bytes()
}
