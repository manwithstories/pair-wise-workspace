package container

import (
	"encoding/binary"
	"strconv"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// Relay MXF universal labels, matching the fixture profile.
var (
	ulHeaderPartitionW = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x04, 0x00}
	ulBodyPartitionW   = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x03, 0x00}
	ulFooterPartitionW = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x05, 0x00}
	ulPrimerW          = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x05, 0x01, 0x00}
	ulTrackSetW        = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x7f, 0x01, 0x00}
	ulIndexTableW      = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x10, 0x01, 0x00}
	ulPictureEssenceW  = []byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01, 0x0d, 0x01, 0x03, 0x01, 0x15, 0x01, 0x05, 0x00}
)

// claimTags lists the relay metadata tags and the claim layer each belongs to.
var claimTags = []struct {
	tag uint16
	key string
	src model.Source
}{
	{tagContainerRot, "rotation", model.SourceContainer},
	{tagBitRot, "rotation", model.SourceBitstream},
	{tagContainerCrop, "crop", model.SourceContainer},
	{tagBitCrop, "crop", model.SourceBitstream},
	{tagContainerPrim, "primaries", model.SourceContainer},
	{tagContainerXfer, "transfer", model.SourceContainer},
	{tagContainerMat, "matrix", model.SourceContainer},
	{tagContainerRange, "range", model.SourceContainer},
	{tagBitPrim, "primaries", model.SourceBitstream},
	{tagBitXfer, "transfer", model.SourceBitstream},
	{tagBitMat, "matrix", model.SourceBitstream},
	{tagBitRange, "range", model.SourceBitstream},
	{tagContainerW, "width", model.SourceContainer},
	{tagContainerH, "height", model.SourceContainer},
	{tagBitW, "width", model.SourceBitstream},
	{tagBitH, "height", model.SourceBitstream},
	{tagBitAperture, "aperture", model.SourceBitstream},
}

// WriteMXF re-serialises a parsed MXF file with every understood claim written
// back. Essence values are copied verbatim and the index table is regenerated
// from the track model so offsets describe the new file exactly.
func WriteMXF(f *File) ([]byte, error) {
	if f == nil || f.Kind != model.MXF {
		return nil, fault.New(-1, 0, fault.KindCorrupt, false, "WriteMXF: not an MXF file")
	}
	if len(f.Tracks) == 0 {
		return nil, fault.New(-1, 0, fault.KindCorrupt, false, "WriteMXF: no tracks")
	}
	var out []byte
	out = append(out, klvBytes(ulHeaderPartitionW, make([]byte, 88))...)
	out = append(out, klvBytes(ulPrimerW, primerBytes())...)
	for _, t := range f.Tracks {
		out = append(out, klvBytes(ulTrackSetW, trackSetBytes(t))...)
	}
	out = append(out, klvBytes(ulBodyPartitionW, make([]byte, 88))...)

	// Emit essence, recording each sample's absolute payload offset.
	valueOffsets := make([][]uint32, len(f.Tracks))
	for ti, t := range f.Tracks {
		valueOffsets[ti] = make([]uint32, len(t.Samples))
		for si, s := range t.Samples {
			payload, err := t.Payload(f.Data, s)
			if err != nil {
				payload = nil
			}
			item := make([]byte, 4, 4+len(payload))
			binary.BigEndian.PutUint32(item, t.ID)
			item = append(item, payload...)
			klv := klvBytes(ulPictureEssenceW, item)
			// The stream offset points at the first payload byte after the
			// key, the BER length and the four-byte track number.
			valueOffsets[ti][si] = uint32(len(out)) + 16 + uint32(len(berLenBytes(len(item)))) + 4
			out = append(out, klv...)
		}
	}
	for ti, t := range f.Tracks {
		out = append(out, klvBytes(ulIndexTableW, indexBytes(t, valueOffsets[ti]))...)
	}
	out = append(out, klvBytes(ulFooterPartitionW, make([]byte, 88))...)
	return out, nil
}

func primerBytes() []byte {
	var out []byte
	out = append(out, localItem(0, nil)...)
	for _, ct := range claimTags {
		out = append(out, localItem(ct.tag, []byte("relay:"+ct.key))...)
	}
	out = append(out, localItem(0x8001, []byte("relay:TrackID"))...)
	out = append(out, localItem(0x8002, []byte("relay:Kind"))...)
	out = append(out, localItem(0x8003, []byte("relay:Codec"))...)
	out = append(out, localItem(0x8004, []byte("relay:EditRateNum"))...)
	out = append(out, localItem(0x8005, []byte("relay:EditRateDen"))...)
	out = append(out, localItem(0x8006, []byte("relay:Duration"))...)
	out = append(out, localItem(0x3f01, []byte("relay:IndexStart"))...)
	out = append(out, localItem(0x3f02, []byte("relay:IndexCount"))...)
	out = append(out, localItem(0x3f03, []byte("relay:IndexEntries"))...)
	return out
}

func trackSetBytes(t *model.Track) []byte {
	var out []byte
	out = append(out, localItem(0x8001, u32b(t.ID))...)
	out = append(out, localItem(0x8002, []byte(t.Kind))...)
	out = append(out, localItem(0x8003, []byte(t.Codec))...)
	out = append(out, localItem(0x8004, u32b(uint32(t.Timebase.Num)))...)
	out = append(out, localItem(0x8005, u32b(uint32(t.Timebase.Den)))...)
	out = append(out, localItem(0x8006, u64b(uint64(t.Duration)))...)
	out = append(out, localItem(0x3f01, u64b(0))...)
	for _, ct := range claimTags {
		v, ok := t.Claims.Get(ct.src, ct.key)
		if !ok {
			continue
		}
		out = append(out, localItem(ct.tag, encodeClaimValue(ct.tag, v))...)
	}
	return out
}

// encodeClaimValue turns a resolved string claim back into the typed tag
// payload the parser expects.
func encodeClaimValue(tag uint16, v string) []byte {
	switch tag {
	case tagContainerRot, tagBitRot:
		n, _ := strconv.Atoi(v)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(int16(n)))
		return b[:]
	case tagContainerCrop, tagBitCrop:
		var l, tp, r, b int
		_, _ = fmtSscan(v, &l, &tp, &r, &b)
		return append(append(append(u16b(uint16(l)), u16b(uint16(tp))...), u16b(uint16(r))...), u16b(uint16(b))...)
	case tagContainerW, tagContainerH, tagBitW, tagBitH:
		n, _ := strconv.Atoi(v)
		return u32b(uint32(n))
	case tagBitAperture:
		w, h := parseWxH(v)
		return append(u32b(uint32(w)), u32b(uint32(h))...)
	case tagContainerPrim, tagContainerXfer, tagContainerMat, tagBitPrim, tagBitXfer, tagBitMat:
		n, _ := strconv.Atoi(v)
		return u16b(uint16(n))
	default:
		return []byte(v)
	}
}

func indexBytes(t *model.Track, offsets []uint32) []byte {
	var entries []byte
	for i, s := range t.Samples {
		off := uint32(0)
		if i < len(offsets) {
			off = offsets[i]
		}
		entries = append(entries, u32b(off)...)
		entries = append(entries, u32b(uint32(s.Duration))...)
		entries = append(entries, u32b(uint32(i))...)
		key := uint32(0)
		if s.Sync {
			key = 1
		}
		entries = append(entries, u32b(key)...)
	}
	var out []byte
	out = append(out, localItem(0x8001, u32b(t.ID))...)
	out = append(out, localItem(0x3f01, u64b(0))...)
	out = append(out, localItem(0x3f02, u32b(uint32(len(t.Samples))))...)
	out = append(out, localItem(0x3f03, entries)...)
	return out
}

func u16b(v uint16) []byte { var b [2]byte; binary.BigEndian.PutUint16(b[:], v); return b[:] }
func u32b(v uint32) []byte { var b [4]byte; binary.BigEndian.PutUint32(b[:], v); return b[:] }
func u64b(v uint64) []byte { var b [8]byte; binary.BigEndian.PutUint64(b[:], v); return b[:] }

// fmtSscan parses "a,b,c,d" without pulling fmt into the hot path.
func fmtSscan(s string, a, b, c, d *int) (int, error) {
	vals := [4]int{}
	n := 0
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if n >= 4 {
				break
			}
			v, err := strconv.Atoi(s[start:i])
			if err != nil {
				return n, err
			}
			vals[n] = v
			n++
			start = i + 1
		}
	}
	*a, *b, *c, *d = vals[0], vals[1], vals[2], vals[3]
	return n, nil
}

// parseWxH parses "WxH".
func parseWxH(s string) (int, int) {
	for i := 0; i < len(s); i++ {
		if s[i] == 'x' || s[i] == 'X' {
			w, _ := strconv.Atoi(s[:i])
			h, _ := strconv.Atoi(s[i+1:])
			return w, h
		}
	}
	return 0, 0
}
