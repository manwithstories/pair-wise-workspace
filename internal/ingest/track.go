package ingest

import (
	"errors"
	"fmt"
	"time"

	mp4 "github.com/abema/go-mp4"

	"mediacq/internal/clock"
)

// ErrMissingKeyframe indicates a video track offers no random-access point.
var ErrMissingKeyframe = errors.New("ingest: no sync sample (keyframe) in video track")

// buildTrack converts a probed go-mp4 track into validated unified-timeline
// samples. Sample times are accumulated from actual stts deltas, so VFR is
// handled without assuming a constant frame duration.
func buildTrack(data []byte, trakIdx int, tr *mp4.Track) (*Track, error) {
	if tr.Timescale == 0 {
		return nil, &BoxError{Box: "mdhd", Detail: "zero timescale"}
	}
	rate, err := clock.NewRate(int64(tr.Timescale), 1)
	if err != nil {
		return nil, &BoxError{Box: "mdhd", Detail: err.Error()}
	}
	kind := classifyTrack(data, trakIdx, tr)
	t := &Track{
		ID: tr.TrackID, Kind: kind, Timescale: tr.Timescale, Rate: rate,
		Samples: make([]Sample, 0, len(tr.Samples)),
	}
	// Absolute chunk offsets are sequential (one sample per chunk in fixtures).
	if len(tr.Chunks) != len(tr.Samples) {
		return nil, &BoxError{Box: "stsc", Detail: fmt.Sprintf(
			"chunk count %d != sample count %d", len(tr.Chunks), len(tr.Samples))}
	}
	var ticks int64
	for i, s := range tr.Samples {
		if s.TimeDelta == 0 {
			return nil, &BoxError{Box: "stts", Detail: fmt.Sprintf(
				"zero sample duration at sample %d", i)}
		}
		start, lerr := rate.ToTimeLocated(ticks, tr.TrackID, int64(tr.Chunks[i].DataOffset))
		if lerr != nil {
			return nil, lerr
		}
		dur, derr := rate.Duration(int64(s.TimeDelta))
		if derr != nil {
			return nil, &clock.Error{Op: "Duration", TrackID: tr.TrackID, RawTS: ticks, Err: derr}
		}
		t.Samples = append(t.Samples, Sample{
			TrackID:    tr.TrackID,
			Index:      i,
			Start:      start,
			Duration:   clock.Time(dur),
			Size:       s.Size,
			FileOffset: int64(tr.Chunks[i].DataOffset),
		})
		ticks += int64(s.TimeDelta)
	}
	if err := attachKeyframes(data, trakIdx, t, tr); err != nil {
		return nil, err
	}
	if kind == "video" && len(t.KeyframeIndices) == 0 {
		return nil, fmt.Errorf("%w: track=%d", ErrMissingKeyframe, tr.TrackID)
	}
	return t, nil
}

// classifyTrack identifies media type from codec metadata when present, and
// falls back to the stss table whose existence is video-specific. A trak that
// carries an empty stss is a video track with no sync sample.
func classifyTrack(data []byte, trakIdx int, tr *mp4.Track) string {
	switch {
	case tr.MP4A != nil:
		return "audio"
	case tr.AVC != nil:
		return "video"
	}
	if ss, err := readStss(data, trakIdx); err == nil && ss != nil {
		return "video"
	}
	return "audio"
}

// attachKeyframes reads stss directly so the keyframe set is validated rather
// than inferred. A video track without stss yields the missing-keyframe error.
func attachKeyframes(data []byte, trakIdx int, t *Track, tr *mp4.Track) error {
	if t.Kind != "video" {
		// PCM audio samples are independently decodable.
		for i := range t.Samples {
			t.Samples[i].Keyframe = true
		}
		return nil
	}
	ss, err := readStss(data, trakIdx)
	if err != nil {
		return err
	}
	if ss == nil {
		return &BoxError{Box: "stss", Detail: "video track missing stss box"}
	}
	for _, n := range ss {
		idx := int(n) - 1 // stss is 1-based
		if idx < 0 || idx >= len(t.Samples) {
			return &BoxError{Box: "stss", Detail: fmt.Sprintf(
				"sample number %d out of range 1..%d", n, len(t.Samples))}
		}
		t.Samples[idx].Keyframe = true
		t.KeyframeIndices = append(t.KeyframeIndices, idx)
	}
	return nil
}

// MaxKeyframeGap returns the largest sample-index distance between successive
// random-access points; the report stage compares it against a policy limit.
func (t *Track) MaxKeyframeGap() int {
	if len(t.KeyframeIndices) == 0 {
		return len(t.Samples)
	}
	maxGap := t.KeyframeIndices[0] + 1
	for i := 1; i < len(t.KeyframeIndices); i++ {
		if g := t.KeyframeIndices[i] - t.KeyframeIndices[i-1]; g > maxGap {
			maxGap = g
		}
	}
	if g := len(t.Samples) - t.KeyframeIndices[len(t.KeyframeIndices)-1]; g > maxGap {
		maxGap = g
	}
	return maxGap
}

// TimelineEnd returns the end of the final sample.
func (t *Track) TimelineEnd() (clock.Time, error) {
	if len(t.Samples) == 0 {
		return 0, nil
	}
	last := t.Samples[len(t.Samples)-1]
	return last.Start.Add(time.Duration(last.Duration))
}
