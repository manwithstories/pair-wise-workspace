package isobmff

import (
	"encoding/binary"
	"math"

	"github.com/pairwise/relay/internal/media"
)

// Trun flag bits used by the deterministic fixtures.
const (
	TrunDataOffsetPresent                  uint32 = 0x000001
	TrunFirstSampleFlagsPresent            uint32 = 0x000004
	TrunSampleDurationPresent              uint32 = 0x000100
	TrunSampleSizePresent                  uint32 = 0x000200
	TrunSampleFlagsPresent                 uint32 = 0x000400
	TrunSampleCompositionTimeOffsetPresent uint32 = 0x000800
)

// MaxSamplesPerFragment bounds trun allocations even for zero-width entries.
const MaxSamplesPerFragment = 65536

// Sample contains the trun fields needed by timeline and boundary checks.
type Sample struct {
	Duration          uint32
	Size              uint32
	Flags             uint32
	CompositionOffset int64
}

// Fragment is the parsed subset of a movie fragment.
type Fragment struct {
	ID                media.FragmentID
	MFHDSequence      uint32
	TFDTVersion       byte
	TFDT              uint64
	TFDTOffset        int64
	TrunFlags         uint32
	Samples           []Sample
	DataOffset        int32
	HasDataOffset     bool
	MdatPayloadOffset int64
	MdatPayloadLength int64
}

// ParseFragment parses moof/mfhd, traf/tfdt and traf/trun and validates that
// trun sample sizes stay inside mdat. A returned Fault identifies the first
// boundary/structural problem; callers isolate this fragment and continue.
func ParseFragment(data []byte, id media.FragmentID) (*Fragment, *media.Fault) {
	tops, f := ParseTopBoxes(data, id)
	if f != nil {
		return nil, f
	}
	moof, ok := firstBox(tops, "moof")
	if !ok {
		return nil, media.NewFault(media.CodeMissingField, id, 0, true, "moof box is required")
	}
	mfhd, ok, f := FindChild(data, moof, "mfhd", id)
	if f != nil {
		return nil, f
	}
	if !ok {
		return nil, media.NewFault(media.CodeMissingField, id, moof.PayloadOffset, true, "mfhd box is required")
	}
	seq, f := parseMFHD(mfhd, id)
	if f != nil {
		return nil, f
	}
	id.MFHD = seq

	traf, ok, f := FindChild(data, moof, "traf", id)
	if f != nil {
		return nil, f
	}
	if !ok {
		return nil, media.NewFault(media.CodeMissingField, id, moof.PayloadOffset, true, "traf box is required")
	}
	tfdtBox, ok, f := FindChild(data, traf, "tfdt", id)
	if f != nil {
		return nil, f
	}
	if !ok {
		return nil, media.NewFault(media.CodeMissingField, id, traf.PayloadOffset, true, "tfdt box is required")
	}
	version, tfdt, tfdtOff, f := parseTFDT(tfdtBox, id)
	if f != nil {
		return nil, f
	}
	trunBox, ok, f := FindChild(data, traf, "trun", id)
	if f != nil {
		return nil, f
	}
	if !ok {
		return nil, media.NewFault(media.CodeMissingField, id, traf.PayloadOffset, true, "trun box is required")
	}
	flags, samples, dataOffset, hasDO, f := parseTRUN(trunBox, id)
	if f != nil {
		return nil, f
	}

	out := &Fragment{
		ID:            id,
		MFHDSequence:  seq,
		TFDTVersion:   version,
		TFDT:          tfdt,
		TFDTOffset:    tfdtOff,
		TrunFlags:     flags,
		Samples:       samples,
		DataOffset:    dataOffset,
		HasDataOffset: hasDO,
	}
	if mdat, ok := firstBox(tops, "mdat"); ok {
		out.MdatPayloadOffset = mdat.PayloadOffset
		out.MdatPayloadLength = int64(len(mdat.Payload))
		var total uint64
		for i, s := range samples {
			sum, overflow := safeU64Add(total, uint64(s.Size))
			if overflow || sum > math.MaxInt64 {
				return nil, media.NewFault(media.CodeIntegerOverflow, id, trunBox.PayloadOffset, false, "sample sizes overflow at sample %d", i)
			}
			total = sum
		}
		if total > uint64(len(mdat.Payload)) {
			// The first byte required by trun but absent from mdat is the
			// first byte after the available mdat payload.
			off := mdat.PayloadOffset + int64(len(mdat.Payload))
			return nil, media.NewFault(media.CodeBoxTruncated, id, off, true, "trun sample sizes need %d mdat bytes, mdat has %d", total, len(mdat.Payload))
		}
	}
	return out, nil
}

func firstBox(boxes []Box, typ string) (Box, bool) {
	for _, b := range boxes {
		if b.Type == typ {
			return b, true
		}
	}
	return Box{}, false
}

func parseMFHD(b Box, id media.FragmentID) (uint32, *media.Fault) {
	p := b.Payload
	if len(p) < 8 {
		return 0, media.NewFault(media.CodeBoxTruncated, id, b.PayloadOffset+int64(len(p)), true, "mfhd needs 8 payload bytes, has %d", len(p))
	}
	if _, f := U32(p[:4], id, "mfhd version/flags", b.PayloadOffset); f != nil {
		return 0, f
	}
	return binary.BigEndian.Uint32(p[4:8]), nil
}

func parseTFDT(b Box, id media.FragmentID) (byte, uint64, int64, *media.Fault) {
	p := b.Payload
	if len(p) < 4 {
		return 0, 0, 0, media.NewFault(media.CodeBoxTruncated, id, b.PayloadOffset+int64(len(p)), true, "tfdt full-box header is truncated")
	}
	vf := binary.BigEndian.Uint32(p[:4])
	version := byte(vf >> 24)
	if version == 1 {
		if len(p) < 12 {
			return 0, 0, 0, media.NewFault(media.CodeBoxTruncated, id, b.PayloadOffset+int64(len(p)), true, "tfdt v1 needs 8 time bytes")
		}
		return version, binary.BigEndian.Uint64(p[4:12]), b.PayloadOffset + 4, nil
	}
	if len(p) < 8 {
		return 0, 0, 0, media.NewFault(media.CodeBoxTruncated, id, b.PayloadOffset+int64(len(p)), true, "tfdt v0 needs 4 time bytes")
	}
	return version, uint64(binary.BigEndian.Uint32(p[4:8])), b.PayloadOffset + 4, nil
}

func parseTRUN(b Box, id media.FragmentID) (uint32, []Sample, int32, bool, *media.Fault) {
	p := b.Payload
	if len(p) < 8 {
		return 0, nil, 0, false, media.NewFault(media.CodeBoxTruncated, id, b.PayloadOffset+int64(len(p)), true, "trun header needs 8 bytes")
	}
	vf := binary.BigEndian.Uint32(p[:4])
	flags := vf & 0x00ffffff
	count := binary.BigEndian.Uint32(p[4:8])
	if count > MaxSamplesPerFragment {
		return 0, nil, 0, false, media.NewFault(media.CodeInputTooLarge, id, b.PayloadOffset+4, false, "trun sample_count %d exceeds %d", count, MaxSamplesPerFragment)
	}
	pos := uint64(8)
	var dataOffset int32
	hasDO := flags&TrunDataOffsetPresent != 0
	if hasDO {
		if uint64(len(p)) < pos+4 {
			return 0, nil, 0, false, trunTrunc(b, p, pos)
		}
		dataOffset = int32(binary.BigEndian.Uint32(p[pos : pos+4]))
		pos += 4
	}
	if flags&TrunFirstSampleFlagsPresent != 0 {
		pos += 4
	}
	entry := uint64(0)
	if flags&TrunSampleDurationPresent != 0 {
		entry += 4
	}
	if flags&TrunSampleSizePresent != 0 {
		entry += 4
	}
	if flags&TrunSampleFlagsPresent != 0 {
		entry += 4
	}
	if flags&TrunSampleCompositionTimeOffsetPresent != 0 {
		entry += 4
	}
	tableBytes := uint64(0)
	if count != 0 {
		if entry > math.MaxUint64/uint64(count) {
			return 0, nil, 0, false, media.NewFault(media.CodeIntegerOverflow, id, b.PayloadOffset, false, "trun sample table size overflows")
		}
		tableBytes = uint64(count) * entry
	}
	need, overflow := safeU64Add(pos, tableBytes)
	if overflow {
		return 0, nil, 0, false, media.NewFault(media.CodeIntegerOverflow, id, b.PayloadOffset, false, "trun sample table size overflows")
	}
	if need > uint64(len(p)) {
		return 0, nil, 0, false, media.NewFault(media.CodeBoxTruncated, id, b.PayloadOffset+int64(len(p)), true, "trun table needs %d bytes, has %d", need, len(p))
	}

	samples := make([]Sample, 0, count)
	for i := uint32(0); i < count; i++ {
		s := Sample{}
		if flags&TrunSampleDurationPresent != 0 {
			s.Duration = binary.BigEndian.Uint32(p[pos : pos+4])
			pos += 4
		}
		if flags&TrunSampleSizePresent != 0 {
			s.Size = binary.BigEndian.Uint32(p[pos : pos+4])
			pos += 4
		}
		if flags&TrunSampleFlagsPresent != 0 {
			s.Flags = binary.BigEndian.Uint32(p[pos : pos+4])
			pos += 4
		}
		if flags&TrunSampleCompositionTimeOffsetPresent != 0 {
			s.CompositionOffset = int64(int32(binary.BigEndian.Uint32(p[pos : pos+4])))
			pos += 4
		}
		samples = append(samples, s)
	}
	return flags, samples, dataOffset, hasDO, nil
}

func trunTrunc(b Box, p []byte, pos uint64) *media.Fault {
	return media.NewFault(media.CodeBoxTruncated, media.FragmentID{}, b.PayloadOffset+int64(len(p)), true, "trun optional field at %d is truncated", pos)
}

func safeU64Add(a, b uint64) (uint64, bool) {
	if a > math.MaxUint64-b {
		return 0, true
	}
	return a + b, false
}
