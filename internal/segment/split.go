package segment

import (
	"fmt"
	"io"
	"os"

	"mediad/internal/model"
)

// Evidence pairs a confirmed segment with its structural walk report.
type Evidence struct {
	Segment model.SegmentRef
	Report  *WalkReport
}

// Validate opens path and walks the declared byte range. The returned
// evidence is non-nil even on error when a prefix of boxes was confirmed, so
// callers can keep recoverable outputs. The MediaError offset is relative to
// the start of the whole file (ByteStart is added for byte-range fragments).
func Validate(path string, seg model.SegmentRef) (*Evidence, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, model.NewMediaError("open", seg.Track, int64(seg.Index), seg.ByteStart, false, err)
	}
	defer f.Close()

	var (
		limit = seg.ByteSize
		base  = seg.ByteStart
	)
	if limit <= 0 {
		info, statErr := f.Stat()
		if statErr != nil {
			return nil, model.NewMediaError("stat", seg.Track, int64(seg.Index), 0, false, statErr)
		}
		limit = info.Size()
	}
	var r io.ReadSeeker = f
	if base != 0 {
		r = io.NewSectionReader(f, base, limit)
		base = 0
	}
	rep, err := WalkStream(r, limit, seg.Track, seg.Index)
	if err != nil {
		me, ok := err.(*model.MediaError)
		if ok && seg.ByteStart != 0 {
			me.Offset += seg.ByteStart
		}
		return &Evidence{Segment: seg, Report: rep}, err
	}
	return &Evidence{Segment: seg, Report: rep}, nil
}

// Cut is one keyframe-aligned output span on the unified timeline.
type Cut struct {
	Track     model.TrackRole `json:"track"`
	Index     int             `json:"index"`
	URI       string          `json:"uri"`
	ByteStart int64           `json:"byte_start"`
	ByteSize  int64           `json:"byte_size"`
	StartMS   int64           `json:"start_ms"`
	EndMS     int64           `json:"end_ms"`
	Keyframe  bool            `json:"keyframe"`
}

// PlanCuts maps requested [from,to) millisecond windows onto confirmed
// segment boundaries. A boundary that no sync segment supports is rejected
// with ErrBoundaryUncertain instead of fabricating an in-fragment cut point.
func PlanCuts(ev []Evidence, fromMS, toMS int64) ([]Cut, error) {
	if fromMS < 0 || toMS <= fromMS {
		return nil, fmt.Errorf("%w: window [%d,%d)", model.ErrBoundaryUncertain, fromMS, toMS)
	}
	var cuts []Cut
	for _, e := range ev {
		seg := e.Segment
		endMS := seg.StartMS + seg.DurationMS
		if endMS <= fromMS || seg.StartMS >= toMS {
			continue
		}
		// The window opens inside this fragment: no proven cut point exists
		// there, so refuse rather than fabricating one.
		if fromMS > seg.StartMS && fromMS < endMS {
			return nil, fmt.Errorf("%w: window opens inside %s#%d", model.ErrBoundaryUncertain, seg.Track, seg.Index)
		}
		if !seg.Sync || (e.Report != nil && !e.Report.Sync()) {
			return nil, fmt.Errorf("%w: %s#%d at %dms", model.ErrBoundaryUncertain, seg.Track, seg.Index, seg.StartMS)
		}
		cuts = append(cuts, Cut{
			Track: seg.Track, Index: seg.Index, URI: seg.URI,
			ByteStart: seg.ByteStart, ByteSize: seg.ByteSize,
			StartMS: seg.StartMS, EndMS: endMS, Keyframe: true,
		})
	}
	if len(cuts) == 0 {
		return nil, fmt.Errorf("%w: no confirmed segment in [%d,%d)", model.ErrBoundaryUncertain, fromMS, toMS)
	}
	return cuts, nil
}

// Extract copies the planned byte span from src to dst in fixed-size chunks,
// keeping peak memory bounded regardless of fragment size.
func Extract(src string, c Cut, dst io.Writer) (int64, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, model.NewMediaError("extract", c.Track, int64(c.Index), c.ByteStart, false, err)
	}
	defer f.Close()
	r := io.NewSectionReader(f, c.ByteStart, c.ByteSize)
	return io.CopyBuffer(dst, r, make([]byte, 64*1024))
}
