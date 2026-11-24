package inspect

import (
	"encoding/binary"

	"mediac/internal/core"
)

// AVC NAL unit type constants (RFC 6184, low 5 bits of the header byte).
const (
	avcNALIDR  = 5
	avcNALSPS  = 7
	avcNALPPS  = 8
)

// HEVC NAL unit type (high 6 bits of the header byte).
const (
	hevcNALIDRWRadl = 19
	hevcNALIDRNL    = 20
	hevcNALCRA      = 21
	hevcNALVPS      = 32
	hevcNALSPS      = 33
	hevcNALPPS      = 34
)

// CodecFamily selects the NAL header interpretation.
type CodecFamily int

const (
	CodecAVC CodecFamily = iota
	CodecHEVC
)

// FamilyFromCodec maps a manifest codec string (avc1./hvc1./hev1.) to a family.
func FamilyFromCodec(codec string) CodecFamily {
	for _, prefix := range []string{"hvc1", "hev1", "hvc2", "hev2"} {
		if len(codec) >= 4 && codec[:4] == prefix {
			return CodecHEVC
		}
	}
	return CodecAVC
}

// analyzeSamples walks length-prefixed NAL units inside mdat and marks
// keyframes. Each sample's bytes are located via the trun size table.
func analyzeSamples(repID string, fragment int, codec string, mdat []byte,
	report *FragmentReport, rec *core.Recorder) {
	family := FamilyFromCodec(codec)
	cursor := 0
	for i := range report.Samples {
		s := &report.Samples[i]
		size := int(s.Size)
		if size <= 0 || cursor+size > len(mdat) {
			report.HasInvalidNAL = true
			f := core.NewFault(core.FaultInvalidNAL, core.SevDamaged,
				"sample size exceeds mdat bounds",
				core.Source{RepID: repID, SegmentIndex: fragment, SampleIndex: i,
					OffsetBytes: int64(cursor)})
			rec.Add(f)
			report.Faults = append(report.Faults, f)
			break
		}
		nalTypes, keyframe, ok := parseNALUs(family, mdat[cursor:cursor+size])
		if !ok {
			report.HasInvalidNAL = true
			f := core.NewFault(core.FaultInvalidNAL, core.SevDamaged,
				"unparseable NAL unit length prefix",
				core.Source{RepID: repID, SegmentIndex: fragment, SampleIndex: i})
			rec.Add(f)
			report.Faults = append(report.Faults, f)
		}
		s.NALTypes = nalTypes
		s.IsKeyframe = keyframe
		cursor += size
	}
}

// parseNALUs splits one sample into length-prefixed NAL units and reports
// their types plus whether the sample holds a keyframe (IDR/CRA).
func parseNALUs(family CodecFamily, sample []byte) (types []byte, keyframe bool, ok bool) {
	pos := 0
	ok = true
	for pos+4 <= len(sample) {
		length := int(binary.BigEndian.Uint32(sample[pos : pos+4]))
		pos += 4
		if length <= 0 || pos+length > len(sample) {
			ok = false
			return types, keyframe, false
		}
		header := sample[pos]
		var nalType byte
		switch family {
		case CodecHEVC:
			nalType = header >> 1
			if nalType == hevcNALIDRWRadl || nalType == hevcNALIDRNL ||
				nalType == hevcNALCRA || nalType == hevcNALVPS ||
				nalType == hevcNALSPS || nalType == hevcNALPPS {
				keyframe = keyframe || nalType == hevcNALIDRWRadl ||
					nalType == hevcNALIDRNL || nalType == hevcNALCRA
			}
		default:
			nalType = header & 0x1f
			if nalType == avcNALIDR || nalType == avcNALSPS || nalType == avcNALPPS {
				keyframe = keyframe || nalType == avcNALIDR
			}
		}
		types = append(types, nalType)
		pos += length
	}
	if pos != len(sample) {
		ok = false
	}
	return types, keyframe, ok
}

// GOPInterval is the gap between two consecutive keyframes on the fragment's
// movie timescale.
type GOPInterval struct {
	FromSample int    `json:"from_sample"`
	ToSample   int    `json:"to_sample"`
	Ticks      uint64 `json:"ticks"`
}

// KeyframeGOPs returns the keyframe-to-keyframe intervals found across the
// given fragment reports. Reports must be in fragment order; each fragment's
// base decode time anchors the ticks on a continuous track timeline.
func KeyframeGOPs(reports []*FragmentReport) []GOPInterval {
	type keyPoint struct {
		decodeT uint64
		sample  int
	}
	var keys []keyPoint
	for _, fr := range reports {
		for _, s := range fr.Samples {
			if s.IsKeyframe {
				keys = append(keys, keyPoint{
					decodeT: fr.BaseDecodeT + sampleOffset(fr, s.Index),
					sample:  s.Index,
				})
			}
		}
	}
	var out []GOPInterval
	for i := 1; i < len(keys); i++ {
		if keys[i].decodeT < keys[i-1].decodeT {
			continue // clock reset at a discontinuity, not a negative GOP
		}
		out = append(out, GOPInterval{
			FromSample: keys[i-1].sample, ToSample: keys[i].sample,
			Ticks: keys[i].decodeT - keys[i-1].decodeT,
		})
	}
	return out
}

// sampleOffset sums durations of samples preceding idx within one fragment.
func sampleOffset(fr *FragmentReport, idx int) uint64 {
	var total uint64
	for i := 0; i < idx && i < len(fr.Samples); i++ {
		total += uint64(fr.Samples[i].Duration)
	}
	return total
}
