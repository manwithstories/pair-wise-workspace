package scan

import (
	"encoding/binary"
)

// SampleSpec describes one trun entry for the fixture builder.
type SampleSpec struct {
	Duration uint32
	Payload  []byte // optional explicit payload; otherwise Size synthetic bytes
	Size     uint32 // used only when Payload is nil
	CTO      int32  // composition time offset (PTS-DTS)
}

// FragmentSpec builds a single-moof fragmented segment used by tests
// and the self-generated demo assets.
type FragmentSpec struct {
	TrackID        uint32
	Sequence       uint32
	BaseDecodeTime uint64
	TFDTv1         bool
	Samples        []SampleSpec
}

// BuildFragment encodes a well-formed single-track fragment:
// moof(mfhd,traf(tfhd,tfdt,trun)) + mdat. Sample payload bytes are
// generated deterministically when not supplied.
func BuildFragment(spec FragmentSpec) []byte {
	// mdat payload
	var mdatPayload []byte
	sizes := make([]uint32, len(spec.Samples))
	for i, s := range spec.Samples {
		if s.Payload != nil {
			sizes[i] = uint32(len(s.Payload))
			mdatPayload = append(mdatPayload, s.Payload...)
			continue
		}
		n := s.Size
		sizes[i] = n
		for j := uint32(0); j < n; j++ {
			mdatPayload = append(mdatPayload, byte(i*31+int(j)*7+int(spec.TrackID)))
		}
	}

	mfhd := buildFullBoxPayload(0, 0)
	mfhd = binary.BigEndian.AppendUint32(mfhd, spec.Sequence)
	mfhdBox := box("mfhd", mfhd)

	// tfhd: default-base-is-moof | sample-description-index |
	// default-sample-flags(non-sync).
	tfhd := buildFullBoxPayload(0, 0x020022)
	tfhd = binary.BigEndian.AppendUint32(tfhd, spec.TrackID)
	tfhd = binary.BigEndian.AppendUint32(tfhd, 1)        // sample_description_index
	tfhd = binary.BigEndian.AppendUint32(tfhd, 0x00010000) // default: non-sync
	tfhdBox := box("tfhd", tfhd)

	var tfdt []byte
	if spec.TFDTv1 {
		tfdt = buildFullBoxPayload(1, 0)
		tfdt = binary.BigEndian.AppendUint64(tfdt, spec.BaseDecodeTime)
	} else {
		tfdt = buildFullBoxPayload(0, 0)
		tfdt = binary.BigEndian.AppendUint32(tfdt, uint32(spec.BaseDecodeTime))
	}
	tfdtBox := box("tfdt", tfdt)

	// trun version 1 (signed composition offsets).
	trunFlags := uint32(0x000001 | 0x000004 | 0x000100 | 0x000200 | 0x000800)
	trun := buildFullBoxPayload(1, trunFlags)
	trun = binary.BigEndian.AppendUint32(trun, uint32(len(spec.Samples)))
	trun = binary.BigEndian.AppendUint32(trun, 0) // data_offset placeholder at payload[8:12]
	trun = binary.BigEndian.AppendUint32(trun, 0) // first_sample_flags: sync
	for i, s := range spec.Samples {
		trun = binary.BigEndian.AppendUint32(trun, s.Duration)
		trun = binary.BigEndian.AppendUint32(trun, sizes[i])
		trun = binary.BigEndian.AppendUint32(trun, uint32(s.CTO))
	}
	trunBox := box("trun", trun)

	trafBox := box("traf", append(append(tfhdBox, tfdtBox...), trunBox...))
	moofBox := box("moof", append(mfhdBox, trafBox...))

	// data_offset is relative to moof start and addresses mdat payload.
	dataOffset := uint32(len(moofBox)) + 8
	binary.BigEndian.PutUint32(trun[8:12], dataOffset)
	// Rebuild the boxes that contain the patched trun.
	trunBox = box("trun", trun)
	trafBox = box("traf", append(append(tfhdBox, tfdtBox...), trunBox...))
	moofBox = box("moof", append(mfhdBox, trafBox...))

	mdatBox := box("mdat", mdatPayload)
	return append(moofBox, mdatBox...)
}

// TruncateInsideTrun returns b cut to a file length that ends
// keepPayload bytes into the trun payload, producing a detectable
// trun_truncated structural fault. It returns nil if no trun box exists.
func TruncateInsideTrun(b []byte, keepPayload int) []byte {
	off := findBox(b, "trun", 0)
	if off < 0 {
		return nil
	}
	payloadStart := off + 8
	end := payloadStart + keepPayload
	if end > len(b) {
		end = len(b)
	}
	return b[:end]
}

// findBox locates the first box of typ at or after start by scanning
// top-level and the moof/traf containers used by the builder.
func findBox(b []byte, typ string, start int) int {
	// Top-level pass.
	for p := start; p+8 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[p : p+4]))
		if string(b[p+4:p+8]) == typ {
			return p
		}
		if size < 8 {
			break
		}
		if string(b[p+4:p+8]) == "moof" || string(b[p+4:p+8]) == "traf" {
			if inner := findBox(b, typ, p+8); inner >= 0 && inner < p+size {
				return inner
			}
		}
		p += size
	}
	return -1
}

func buildFullBoxPayload(version byte, flags uint32) []byte {
	return []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

func box(typ string, payload []byte) []byte {
	size := uint32(8 + len(payload))
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], size)
	copy(out[4:8], typ)
	return append(out, payload...)
}
