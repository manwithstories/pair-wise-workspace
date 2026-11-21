package index

import "archivecheck.local/archivecheck/internal/remux"

// FromOutput builds the keyframe index from the writer's completed sample
// table. This is the index the CLI reports on: every entry is a sample that
// actually exists in the output file at a known offset.
func FromOutput(sum *remux.Summary) Index {
	ix := Index{Rebuilt: true}
	for _, m := range sum.Manifest {
		if !m.Keyframe {
			continue
		}
		off := sum.MdatOffset
		if list, ok := sum.SampleOffsets[m.Track]; ok && int(m.OutputSample) <= len(list) {
			off = list[m.OutputSample-1]
		}
		ix.Entries = append(ix.Entries, Entry{
			Track:        m.Track,
			Tick:         m.Tick,
			SourceOffset: off,
			FrameIndex:   0,
			OutputSample: int(m.OutputSample),
			FromCues:     false,
		})
	}
	return ix
}
