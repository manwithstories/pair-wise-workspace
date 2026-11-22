package mp4

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	gomp4 "github.com/abema/go-mp4"

	"medialine/internal/clock"
	"medialine/internal/fault"
	"medialine/internal/model"
)

// MaxSamplesPerTrack bounds sample-table expansion so a hostile stsz cannot
// trigger an unbounded allocation.
const MaxSamplesPerTrack = 4_000_000

// trakBoxes holds the raw sample-table boxes for one trak.
type trakBoxes struct {
	tkhd   *gomp4.Tkhd
	mdhd   *gomp4.Mdhd
	hdlr   *gomp4.Hdlr
	elst   *gomp4.Elst
	stts   *gomp4.Stts
	stsc   *gomp4.Stsc
	stsz   *gomp4.Stsz
	stco   *gomp4.Stco
	co64   *gomp4.Co64
	stss   *gomp4.Stss
	offset uint64
}

// trackID returns the ID named by tkhd, or 0 when tkhd was unreadable.
func (tb *trakBoxes) trackID() int {
	if tb.tkhd == nil {
		return 0
	}
	return int(tb.tkhd.TrackID)
}

// buildTrack converts one trak's sample tables into a model track. Recoverable
// problems are returned as issues; a nil track means the tables were too
// damaged to describe samples at all.
func buildTrack(tb *trakBoxes, source string) (*model.Track, []*fault.Error) {
	trackID := tb.trackID()
	if trackID <= 0 {
		trackID = 1
	}
	var issues []*fault.Error
	add := func(e *fault.Error) { issues = append(issues, e) }

	if tb.mdhd == nil || tb.stsz == nil || (tb.stco == nil && tb.co64 == nil) || tb.stts == nil {
		add(fault.New(fault.StageIndex, fault.KindMissingIndex,
			"track %d is missing mdhd/stsz/stts/stco", trackID).At(trackID, int64(tb.offset)))
		return nil, issues
	}
	if tb.mdhd.Timescale == 0 {
		add(fault.New(fault.StageIndex, fault.KindAmbiguousTime,
			"track %d has zero timescale", trackID).At(trackID, int64(tb.offset)))
		return nil, issues
	}
	count := int(tb.stsz.SampleCount)
	if count > MaxSamplesPerTrack {
		add(fault.New(fault.StageIndex, fault.KindInvalidLength,
			"track %d declares %d samples, limit %d", trackID, count, MaxSamplesPerTrack).
			At(trackID, int64(tb.offset)))
		return nil, issues
	}

	sizes, err := sampleSizes(tb.stsz, count)
	if err != nil {
		add(fault.From(err, fault.StageIndex, fault.KindInvalidLength).At(trackID, int64(tb.offset)))
		return nil, issues
	}
	offsets, err := sampleOffsets(tb, sizes, count)
	if err != nil {
		add(fault.From(err, fault.StageIndex, fault.KindInvalidLength).At(trackID, int64(tb.offset)))
		return nil, issues
	}
	durations, err := sampleDurations(tb.stts, count)
	if err != nil {
		add(fault.From(err, fault.StageIndex, fault.KindInvalidLength).At(trackID, int64(tb.offset)))
		return nil, issues
	}

	keyframes := keyframeSet(tb.stss, count)
	startUS, ok := editStartUS(tb, trackID, add)
	if !ok {
		return nil, issues
	}

	kind := model.KindVideo
	codec := "vide"
	if tb.hdlr != nil {
		if string(tb.hdlr.HandlerType[:]) == "soun" {
			kind = model.KindAudio
		}
		codec = strings.TrimRight(string(tb.hdlr.HandlerType[:]), "\x00")
	}

	tr := &model.Track{ID: trackID, Kind: kind, Timescale: tb.mdhd.Timescale, Source: source, Codec: codec}
	dtsTicks := int64(0)
	for i := 0; i < count; i++ {
		us, e := clock.TicksToMicros(dtsTicks, uint64(tb.mdhd.Timescale))
		if e != nil {
			add(fault.From(e, fault.StageClock, fault.KindAmbiguousTime).At(trackID, int64(i)))
			break
		}
		durUS, e := clock.TicksToMicros(int64(durations[i]), uint64(tb.mdhd.Timescale))
		if e != nil {
			add(fault.From(e, fault.StageClock, fault.KindAmbiguousTime).At(trackID, int64(i)))
			break
		}
		tr.Samples = append(tr.Samples, model.Sample{
			Number:     i + 1,
			DTSUS:      us + startUS,
			PTSUS:      us + startUS,
			DurationUS: durUS,
			Size:       sizes[i],
			Offset:     offsets[i],
			Keyframe:   keyframes[i],
		})
		dtsTicks += int64(durations[i])
	}
	if err := tr.Validate(); err != nil {
		fe := fault.From(err, fault.StageParse, fault.KindCorruptSample)
		add(fe)
	}
	return tr, issues
}

// editStartUS derives the timeline origin from the first edit-list entry,
// which models a non-zero media start. A media_time of -1 is an empty edit and
// is skipped.
func editStartUS(tb *trakBoxes, trackID int, add func(*fault.Error)) (int64, bool) {
	if tb.elst == nil || len(tb.elst.Entries) == 0 {
		return 0, true
	}
	mediaTime := tb.elst.GetMediaTime(0)
	if mediaTime == -1 {
		for i := 1; i < len(tb.elst.Entries); i++ {
			if mt := tb.elst.GetMediaTime(i); mt >= 0 {
				mediaTime = mt
				break
			}
		}
	}
	if mediaTime == -1 {
		return 0, true
	}
	us, err := clock.TicksToMicros(mediaTime, uint64(tb.mdhd.Timescale))
	if err != nil {
		add(fault.From(err, fault.StageClock, fault.KindAmbiguousTime).At(trackID, int64(tb.offset)))
		return 0, true
	}
	return -us, true
}

func sampleSizes(stsz *gomp4.Stsz, count int) ([]int, error) {
	sizes := make([]int, count)
	if stsz.SampleSize != 0 {
		for i := range sizes {
			sizes[i] = int(stsz.SampleSize)
		}
		return sizes, nil
	}
	if len(stsz.EntrySize) < count {
		return nil, fmt.Errorf("stsz declares %d samples but has %d sizes", count, len(stsz.EntrySize))
	}
	for i := range sizes {
		sizes[i] = int(stsz.EntrySize[i])
	}
	return sizes, nil
}

func sampleOffsets(tb *trakBoxes, sizes []int, count int) ([]int64, error) {
	var chunks []uint64
	if tb.stco != nil {
		for _, o := range tb.stco.ChunkOffset {
			chunks = append(chunks, uint64(o))
		}
	} else {
		chunks = append(chunks, tb.co64.ChunkOffset...)
	}
	perChunk, err := samplesPerChunk(tb.stsc, len(chunks))
	if err != nil {
		return nil, err
	}
	offsets := make([]int64, count)
	n := 0
	for c, spc := range perChunk {
		off := chunks[c]
		for j := uint32(0); j < spc && n < count; j++ {
			offsets[n] = int64(off)
			off += uint64(sizes[n])
			n++
		}
		if n >= count {
			break
		}
	}
	if n < count {
		return nil, fmt.Errorf("chunk table only covers %d of %d samples", n, count)
	}
	return offsets, nil
}

func samplesPerChunk(stsc *gomp4.Stsc, chunkCount int) ([]uint32, error) {
	out := make([]uint32, chunkCount)
	if stsc == nil || len(stsc.Entries) == 0 {
		for i := range out {
			out[i] = 1
		}
		return out, nil
	}
	for i, ent := range stsc.Entries {
		if ent.FirstChunk == 0 || int(ent.FirstChunk) > chunkCount {
			return nil, fmt.Errorf("stsc entry %d references chunk %d of %d", i, ent.FirstChunk, chunkCount)
		}
		last := chunkCount
		if i+1 < len(stsc.Entries) {
			last = int(stsc.Entries[i+1].FirstChunk) - 1
		}
		for c := int(ent.FirstChunk) - 1; c < last; c++ {
			out[c] = ent.SamplesPerChunk
		}
	}
	return out, nil
}

func sampleDurations(stts *gomp4.Stts, count int) ([]uint64, error) {
	out := make([]uint64, 0, count)
	for _, ent := range stts.Entries {
		for i := uint32(0); i < ent.SampleCount; i++ {
			out = append(out, uint64(ent.SampleDelta))
			if len(out) > count {
				return nil, errors.New("stts describes more samples than stsz")
			}
		}
	}
	if len(out) < count {
		return nil, fmt.Errorf("stts describes %d of %d samples", len(out), count)
	}
	return out, nil
}

func keyframeSet(stss *gomp4.Stss, count int) []bool {
	out := make([]bool, count)
	if stss == nil {
		// Absence of stss means every sample is a sync sample (audio).
		for i := range out {
			out[i] = true
		}
		return out
	}
	for _, n := range stss.SampleNumber {
		if n >= 1 && int(n) <= count {
			out[int(n)-1] = true
		}
	}
	return out
}

// TrackSummary renders a compact per-track line for the demo output.
func (f *File) TrackSummary() string {
	var b strings.Builder
	for i, t := range f.Tracks {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString("t" + strconv.Itoa(t.ID) + ":" + string(t.Kind) + ":" + strconv.Itoa(len(t.Samples)) + "s")
	}
	return b.String()
}
