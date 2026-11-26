package demux

import (
	"relaycheck/internal/clock"
)

// Sample is one extracted media sample; Data aliases the segment's mdat
// buffer and must not be mutated.
type Sample struct {
	Duration uint32
	Size     uint32
	Flags    uint32
	CTO      int32
	Data     []byte
}

// ExtractAll resolves every track fragment's sample table against the
// segment's mdat payload, following the standard data-offset rules
// (explicit trun data-offset, else concatenation after the previous
// fragment's data). Out-of-range tables are reported with the absolute
// file offset of the offending sample.
func (s *Segment) ExtractAll(seq uint32) ([][]Sample, *clock.Fault) {
	out := make([][]Sample, len(s.Traf))
	implicit := s.MdatOffset
	for i := range s.Traf {
		tf := &s.Traf[i]
		abs := implicit
		if tf.HasDataOffset {
			abs = tf.MoofOffset + int64(tf.DataOffset)
		}
		rel := abs - s.MdatOffset
		samples := make([]Sample, 0, len(tf.Entries))
		pos := rel
		for _, e := range tf.Entries {
			smp := Sample{
				Duration: tf.DefaultDuration,
				Size:     tf.DefaultSize,
				Flags:    tf.DefaultFlags,
			}
			if tf.TrunFlags&0x100 != 0 {
				smp.Duration = e.SampleDuration
			}
			if tf.TrunFlags&0x200 != 0 {
				smp.Size = e.SampleSize
			}
			if tf.TrunFlags&0x400 != 0 {
				smp.Flags = e.SampleFlags
			}
			if tf.TrunFlags&0x800 != 0 {
				if tf.TrunVersion == 0 {
					smp.CTO = int32(e.SampleCompositionTimeOffsetV0)
				} else {
					smp.CTO = e.SampleCompositionTimeOffsetV1
				}
			}
			if smp.Duration == 0 {
				f := clock.NewFault(clock.FaultSampleTable, seq, tf.TrackID, abs,
					"sample duration is zero")
				return nil, &f
			}
			if pos < 0 || pos+int64(smp.Size) > int64(len(s.Mdat)) {
				f := clock.NewFault(clock.FaultBoxBound, seq, tf.TrackID, abs,
					"sample data out of mdat bounds")
				return nil, &f
			}
			smp.Data = s.Mdat[pos : pos+int64(smp.Size)]
			pos += int64(smp.Size)
			samples = append(samples, smp)
		}
		out[i] = samples
		implicit = s.MdatOffset + pos
	}
	return out, nil
}
