package mux

import (
	"fmt"

	"mediamerge/internal/clock"
	"mediamerge/internal/demux"
	"mediamerge/internal/model"
	"mediamerge/internal/mp4util"
)

func (w *Writer) buildMoov() ([]byte, error) {
	movieTS := w.plan.OutputTimescale()
	var movieDur uint64
	traks := make([][]byte, 0, len(w.plan.Tracks))
	for _, t := range w.plan.Tracks {
		var sum uint64
		for _, s := range t.Samples {
			sum += uint64(s.Duration)
		}
		if t.TrackDuration > sum {
			sum = t.TrackDuration
		}
		scaled, err := clock.Scale(sum, t.Timescale, movieTS)
		if err != nil {
			return nil, model.WrapError(model.StageMux, model.CodeOverflow, -1, t.TargetID, false, err,
				"scale track %d duration", t.TargetID)
		}
		if scaled > movieDur {
			movieDur = scaled
		}
		traks = append(traks, w.trak(t, movieTS))
	}
	mvhd := mp4util.FullBox("mvhd", 0, 0, mp4util.Cat(
		mp4util.U32(0), mp4util.U32(0), mp4util.U32(movieTS), mp4util.U32(uint32(movieDur)),
		mp4util.U32(0x00010000), mp4util.U16(0x0100), mp4util.U16(0), mp4util.Zeros(8),
		mp4util.Matrix(), mp4util.Zeros(24), mp4util.U32(uint32(len(w.plan.Tracks)+1)),
	))
	parts := [][]byte{mvhd}
	parts = append(parts, traks...)
	return mp4util.Box("moov", parts...), nil
}

func (w *Writer) trak(t *demux.Track, movieTS uint32) []byte {
	var dur uint64
	for _, s := range t.Samples {
		dur += uint64(s.Duration)
	}
	scaled, err := clock.Scale(dur, t.Timescale, movieTS)
	if err != nil {
		scaled = dur
	}
	vol := uint16(0)
	if t.MediaType == model.MediaAudio {
		vol = 0x0100
	}
	tkhd := mp4util.FullBox("tkhd", 0, 0x7, mp4util.Cat(
		mp4util.U32(0), mp4util.U32(0), mp4util.U32(t.TargetID), mp4util.U32(0),
		mp4util.U32(uint32(scaled)), mp4util.Zeros(8), mp4util.U16(0), mp4util.U16(0),
		mp4util.U16(vol), mp4util.U16(0), mp4util.Matrix(),
		mp4util.U32(uint32(t.Width)<<16), mp4util.U32(uint32(t.Height)<<16),
	))
	mdhd := mp4util.FullBox("mdhd", 0, 0, mp4util.Cat(
		mp4util.U32(0), mp4util.U32(0), mp4util.U32(t.Timescale), mp4util.U32(uint32(dur)),
		mp4util.U16(mp4util.Language(t.Language)), mp4util.U16(0),
	))
	handler := t.HandlerType
	if handler == "" {
		if t.MediaType == model.MediaAudio {
			handler = "soun"
		} else {
			handler = "vide"
		}
	}
	hdlr := mp4util.FullBox("hdlr", 0, 0, mp4util.Cat(
		mp4util.U32(0), mp4util.FourCC(handler), mp4util.Zeros(12), []byte("MediaMerge\x00"),
	))
	dinf := mp4util.Box("dinf", mp4util.FullBox("dref", 0, 0, mp4util.U32(1),
		mp4util.FullBox("url ", 0, 1)))

	var sizes []uint32
	var offsets []uint64
	var durations []uint32
	var ctsOffsets []int64
	var syncNums []uint32
	allSync := true
	trackIdx := -1
	for i, pt := range w.plan.Tracks {
		if pt == t {
			trackIdx = i
			break
		}
	}
	for si, s := range t.Samples {
		sizes = append(sizes, s.Size)
		durations = append(durations, s.Duration)
		ctsOffsets = append(ctsOffsets, s.CTSOffset)
		if trackIdx >= 0 {
			offsets = append(offsets, uint64(w.positions[trackIdx][si]))
		}
		if t.MediaType == model.MediaVideo {
			if s.Sync {
				syncNums = append(syncNums, uint32(si+1))
			} else {
				allSync = false
			}
		}
	}
	stsdEntry := t.SampleEntry
	if len(stsdEntry) == 0 {
		return nil
	}
	stsd := mp4util.FullBox("stsd", 0, 0, mp4util.U32(1), stsdEntry)
	sttsRuns := runsDurations(durations)
	stts := mp4util.FullBox("stts", 0, 0, mp4util.U32(uint32(len(sttsRuns)/8)), sttsRuns)
	parts := [][]byte{stsd, stts}
	if hasNonZero(ctsOffsets) {
		runs := runsCTTS(ctsOffsets)
		parts = append(parts, mp4util.FullBox("ctts", 1, 0, mp4util.U32(uint32(len(runs)/8)), runs))
	}
	parts = append(parts,
		mp4util.FullBox("stsc", 0, 0, mp4util.U32(1), mp4util.U32(1), mp4util.U32(1), mp4util.U32(1)),
		mp4util.FullBox("stsz", 0, 0, mp4util.U32(0), mp4util.U32(uint32(len(sizes))), u32s(sizes)),
		mp4util.FullBox("co64", 0, 0, mp4util.U32(uint32(len(offsets))), u64s(offsets)),
	)
	if t.MediaType == model.MediaVideo && len(syncNums) > 0 && !allSync {
		parts = append(parts, mp4util.FullBox("stss", 0, 0, mp4util.U32(uint32(len(syncNums))), u32s(syncNums)))
	}
	var mediaHeader []byte
	if t.MediaType == model.MediaAudio {
		mediaHeader = mp4util.FullBox("smhd", 0, 0, mp4util.U16(0), mp4util.U16(0))
	} else {
		mediaHeader = mp4util.FullBox("vmhd", 0, 1, mp4util.U16(0), mp4util.U16(0), mp4util.U16(0), mp4util.U16(0))
	}
	stbl := mp4util.Box("stbl", parts...)
	minf := mp4util.Box("minf", mediaHeader, dinf, stbl)
	return mp4util.Box("trak", tkhd, mp4util.Box("mdia", mdhd, hdlr, minf))
}

func runsDurations(d []uint32) []byte {
	var out []byte
	for i := 0; i < len(d); {
		j := i
		for j < len(d) && d[j] == d[i] {
			j++
		}
		out = append(out, mp4util.U32(uint32(j-i))...)
		out = append(out, mp4util.U32(d[i])...)
		i = j
	}
	return out
}

func runsCTTS(o []int64) []byte {
	var out []byte
	for i := 0; i < len(o); {
		j := i
		for j < len(o) && o[j] == o[i] {
			j++
		}
		out = append(out, mp4util.U32(uint32(j-i))...)
		out = append(out, mp4util.U32(uint32(int32(o[i])))...)
		i = j
	}
	return out
}

func hasNonZero(v []int64) bool {
	for _, x := range v {
		if x != 0 {
			return true
		}
	}
	return false
}

func u32s(v []uint32) []byte {
	out := make([]byte, 0, len(v)*4)
	for _, x := range v {
		out = append(out, mp4util.U32(x)...)
	}
	return out
}

func u64s(v []uint64) []byte {
	out := make([]byte, 0, len(v)*8)
	for _, x := range v {
		out = append(out, mp4util.U64(x)...)
	}
	return out
}

// Describe returns a compact deterministic description of the layout.
func (l *Layout) Describe() string {
	return fmt.Sprintf("header=%d payload=%d movie_end=%d", l.HeaderLen, l.PayloadLen, l.MovieEnd)
}
