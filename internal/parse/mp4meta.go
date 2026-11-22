package parse

import (
	mp4 "github.com/abema/go-mp4"

	"avscan/internal/model"
)

// readElst preserves edit-list entries in the movie timescale.
func (st *mp4State) readElst(h *mp4.ReadHandle) {
	if st.cur == nil {
		return
	}
	box, _, err := h.ReadPayload()
	if err != nil {
		st.faults = append(st.faults, byteFault(h, st.source, "elst: "+err.Error()))
		return
	}
	elst, ok := box.(*mp4.Elst)
	if !ok {
		return
	}
	for _, e := range elst.Entries {
		entry := model.EditEntry{MediaRate: int32(e.MediaRateInteger) << 16}
		if elst.GetVersion() == 1 {
			entry.SegmentDuration = e.SegmentDurationV1
			entry.MediaTime = e.MediaTimeV1
		} else {
			entry.SegmentDuration = uint64(e.SegmentDurationV0)
			entry.MediaTime = int64(e.MediaTimeV0)
		}
		st.cur.Edits = append(st.cur.Edits, entry)
	}
}

// readColr records colour metadata verbatim; missing fields are handled by
// the timeline layer's separate missing/conflict counters.
func (st *mp4State) readColr(h *mp4.ReadHandle) {
	if st.cur == nil {
		return
	}
	box, _, err := h.ReadPayload()
	if err != nil {
		st.faults = append(st.faults, byteFault(h, st.source, "colr: "+err.Error()))
		return
	}
	colr, ok := box.(*mp4.Colr)
	if !ok {
		return
	}
	st.cur.Video.HasColr = true
	st.cur.Video.ColourType = string(colr.ColourType[:])
	st.cur.Video.ColourPrimaries = colr.ColourPrimaries
	st.cur.Video.TransferCharacteristics = colr.TransferCharacteristics
	st.cur.Video.MatrixCoefficients = colr.MatrixCoefficients
	st.cur.Video.FullRange = colr.FullRangeFlag
}

// readSampleEntry captures audio/visual entry fields while descending stsd.
func (st *mp4State) readSampleEntry(h *mp4.ReadHandle, t mp4.BoxType) {
	if st.cur == nil {
		return
	}
	box, _, err := h.ReadPayload()
	if err != nil {
		st.faults = append(st.faults, byteFault(h, st.source,
			"sample entry "+t.String()+": "+err.Error()))
		return
	}
	switch b := box.(type) {
	case *mp4.AudioSampleEntry:
		st.cur.Audio.Present = true
		st.cur.Audio.ChannelCount = b.ChannelCount
		st.cur.Audio.SampleRate = b.SampleRate
		st.cur.Audio.SampleSize = b.SampleSize
	case *mp4.VisualSampleEntry:
		st.cur.Video.Present = true
		st.cur.Video.Width = b.Width
		st.cur.Video.Height = b.Height
	}
}
