package index

import "archivecheck.local/archivecheck/internal/remux"

// FromOutput builds a keyframe index from the writer's completed sample table.
// It is used where the source Matroska file is no longer available (for
// example when indexing a finished archive copy) and every entry refers to a
// sample that exists in the output at a recorded offset.
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
			OutputSample: int(m.OutputSample),
		})
	}
	return ix
}
