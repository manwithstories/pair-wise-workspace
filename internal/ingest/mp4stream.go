package ingest

import (
	"fmt"
	"io"

	"mediarun/internal/mediaerr"
)

// Next returns the next sample event in interleaved file order: among the
// tracks that still have samples it picks the one whose next payload starts
// at the lowest absolute offset. Payload is a bounded io.SectionReader so
// callers stream at most one sample at a time instead of loading the file.
func (s *mp4Streamer) Next() (*SampleEvent, error) {
	var pick *mp4Track
	pickIdx := -1
	var pickOffset int64 = 1<<63 - 1
	for i, tr := range s.tracks {
		if tr.next >= len(tr.samples) {
			continue
		}
		off := tr.samples[tr.next].offset
		if off < pickOffset {
			pickOffset = off
			pick = tr
			pickIdx = i
		}
	}
	if pick == nil {
		return nil, io.EOF
	}
	fs := pick.samples[pick.next]
	if err := pick.validate(fs); err != nil {
		return nil, err
	}
	ev := &SampleEvent{
		TrackID:    pick.desc.ID,
		Index:      int64(pick.next),
		DataOffset: fs.offset,
		Size:       fs.size,
		DTS:        fs.dts,
		PTS:        fs.pts,
		Duration:   fs.duration,
		Timebase:   pick.tb,
		Payload:    io.NewSectionReader(s.file, fs.offset, fs.size),
	}
	pick.next++
	_ = pickIdx
	return ev, nil
}

// validate enforces per-track timing invariants. DTS must never regress and
// durations must be non-negative; both failures are structured errors with
// the track and offending sample position.
func (t *mp4Track) validate(fs flatSample) error {
	if fs.duration < 0 {
		return mediaerr.New(mediaerr.CodeNegativeDuration, fmt.Sprintf(
			"sample %d declares negative duration %d", t.next, fs.duration)).
			At("mp4", t.desc.ID, fs.offset)
	}
	if t.hasDTS && fs.dts < t.dtsPrev {
		return mediaerr.New(mediaerr.CodeDTSRegression, fmt.Sprintf(
			"sample %d DTS %d < previous %d", t.next, fs.dts, t.dtsPrev)).
			At("mp4", t.desc.ID, fs.offset)
	}
	t.hasDTS = true
	t.dtsPrev = fs.dts
	return nil
}
