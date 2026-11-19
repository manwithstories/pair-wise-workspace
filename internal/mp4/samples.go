package mp4

import (
	"fmt"

	"archive/internal/errs"
)

// buildSamples materializes per-track samples from the collected moof/traf
// state, slicing compressed payloads directly out of the segment buffer.
// Sample offsets that fall outside the file mean a truncated segment and
// are reported with the offending byte offset.
func (d *demuxer) buildSamples() error {
	for _, moof := range d.moofs {
		for _, traf := range moof.trafs {
			t := d.trackByID(traf.trackID)
			t.ClockBits = traf.clockBits
			base := moof.offset
			if traf.hasBaseData {
				base = traf.baseDataOff
			}
			dts := traf.baseDTS
			mask := uint64(0)
			if traf.clockBits == 32 {
				mask = 0xffffffff // a 32-bit media clock wraps at 2^32
			}
			for _, trun := range traf.truns {
				off := int64(base)
				if trun.hasOff {
					off += int64(trun.dataOff)
				}
				for i, e := range trun.entries {
					size := traf.defSize
					if e.SampleSize != 0 {
						size = e.SampleSize
					}
					dur := traf.defDur
					if e.SampleDuration != 0 {
						dur = e.SampleDuration
					}
					flags := traf.defFlags
					if trun.hasFirst && i == 0 {
						flags = trun.firstFlgs
					} else if e.SampleFlags != 0 {
						flags = e.SampleFlags
					}
					if size == 0 {
						return errs.Newf(errs.KindDemux, d.asset,
							fmt.Sprintf("%s track %d", d.name, traf.trackID),
							"sample %d has zero size", i)
					}
					end := off + int64(size)
					if off < 0 || end > int64(len(d.data)) {
						return errs.Newf(errs.KindDemux, d.asset,
							fmt.Sprintf("%s box offset %d", d.name, off),
							"sample data [%d,%d) exceeds segment size %d (truncated?)",
							off, end, len(d.data))
					}
					var cto int32
					if e.SampleCompositionTimeOffsetV1 != 0 {
						cto = e.SampleCompositionTimeOffsetV1
					} else {
						cto = int32(e.SampleCompositionTimeOffsetV0)
					}
					t.Samples = append(t.Samples, Sample{
						DTS:       int64(dts),
						CTSOffset: cto,
						Duration:  dur,
						Keyframe:  flags&0x00010000 == 0, // sample_is_non_sync_sample
						Data:      d.data[off:end],
					})
					dts += uint64(dur)
					if mask != 0 {
						dts &= mask
					}
					off = end
				}
			}
		}
	}
	return nil
}
