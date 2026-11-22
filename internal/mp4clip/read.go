package mp4clip

import (
	"io"

	"github.com/abema/go-mp4"

	"mediakernel/internal/model"
)

// Timing is the parsed presentation-timing description of one clip track.
type Timing struct {
	TrackID       uint32
	Timescale     uint32
	Handler       string
	SampleCount   int
	TotalDuration uint32
	// SampleTimes holds each sample's start time in track timescale units.
	SampleTimes  []uint32
	SampleDeltas []uint32
	SampleSizes  []uint32
	ChunkOffsets []uint32
}

// ReadTiming parses moov tables back out of an MP4 stream and reconstructs
// per-sample start times from the stts run table.
func ReadTiming(r io.ReadSeeker) (*Timing, error) {
	paths := []mp4.BoxPath{
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()},
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()},
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStts()},
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()},
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStco()},
	}
	extracted, err := mp4.ExtractBoxesWithPayload(r, nil, paths)
	if err != nil {
		return nil, &model.MediaError{
			Category: model.CatRTPHeader,
			Detail:   "mp4 structure parse failed: " + err.Error(),
			Decision: model.DecRejected,
		}
	}
	t := &Timing{}
	var deltas []uint32
	for _, item := range extracted {
		switch p := item.Payload.(type) {
		case *mp4.Mdhd:
			t.Timescale = p.Timescale
			t.TotalDuration = p.DurationV0
		case *mp4.Hdlr:
			t.Handler = string(p.HandlerType[:])
		case *mp4.Stts:
			deltas = expandStts(p)
		case *mp4.Stsz:
			t.SampleSizes = append([]uint32(nil), p.EntrySize...)
			t.SampleCount = int(p.SampleCount)
		case *mp4.Stco:
			t.ChunkOffsets = append([]uint32(nil), p.ChunkOffset...)
		}
	}
	if t.Timescale == 0 || len(deltas) == 0 {
		return nil, &model.MediaError{
			Category: model.CatAlignment,
			Detail:   "mp4 clip missing mdhd timescale or stts table",
			Decision: model.DecRejected,
		}
	}
	t.SampleDeltas = deltas
	t.SampleTimes = cumulativeStarts(deltas)
	return t, nil
}

func expandStts(stts *mp4.Stts) []uint32 {
	total := 0
	for _, e := range stts.Entries {
		total += int(e.SampleCount)
	}
	out := make([]uint32, 0, total)
	for _, e := range stts.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			out = append(out, e.SampleDelta)
		}
	}
	return out
}

func cumulativeStarts(deltas []uint32) []uint32 {
	out := make([]uint32, len(deltas))
	var acc uint32
	for i, d := range deltas {
		out[i] = acc
		acc += d
	}
	return out
}

// VerifyTiming checks that parsed deltas match the expected schedule exactly,
// that sample times are strictly monotonic, and that sizes/offsets agree with
// the payload layout written by WriteClip.
func (t *Timing) VerifyTiming(wantDeltas []uint32, wantSizes []uint32) error {
	if len(wantDeltas) != len(t.SampleDeltas) {
		return timingError("sample count mismatch between written and parsed clip")
	}
	for i, d := range wantDeltas {
		if t.SampleDeltas[i] != d {
			return timingError("stts delta mismatch at sample")
		}
		if i > 0 && t.SampleTimes[i] <= t.SampleTimes[i-1] {
			return timingError("sample times are not strictly monotonic")
		}
	}
	if len(wantSizes) != len(t.SampleSizes) {
		return timingError("stsz sample count mismatch")
	}
	for i, s := range wantSizes {
		if t.SampleSizes[i] != s {
			return timingError("stsz size mismatch at sample")
		}
	}
	if len(t.ChunkOffsets) == 0 {
		return timingError("stco contains no chunk offset")
	}
	return nil
}

func timingError(detail string) error {
	return &model.MediaError{
		Category: model.CatSync,
		Detail:   detail,
		Decision: model.DecRejected,
	}
}
