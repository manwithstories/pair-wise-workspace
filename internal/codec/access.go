// Package codec validates H.264, H.265, and AV1 access-unit boundaries and
// their consistency with the per-AU index. It validates structure only; it
// does not decode or transcode pictures.
package codec

import (
	"errors"
	"fmt"

	"mediad/internal/timeline"
)

// MaxAUBytes is the hard per-access-unit limit.
const MaxAUBytes = 8 << 20

type Codec string

const (
	H264 Codec = "h264"
	H265 Codec = "h265"
	AV1  Codec = "av1"
)

func (c Codec) Valid() bool {
	switch c {
	case H264, H265, AV1:
		return true
	default:
		return false
	}
}

// Report is the isolation result for one indexed access unit.
type Report struct {
	Codec            Codec  `json:"codec"`
	AUIndex          int    `json:"au_index"`
	Offset           int64  `json:"offset"`
	Length           int    `json:"length"`
	ClaimedKeyframe  bool   `json:"claimed_keyframe"`
	DetectedKeyframe bool   `json:"detected_keyframe"`
	Units            int    `json:"units"`
	Valid            bool   `json:"valid"`
	FirstErrorByte   int64  `json:"first_error_byte"`
	Error            string `json:"error,omitempty"`
}

type boundaryInfo struct {
	units    int
	keyframe bool
	errAt    int // relative to AU; -1 when unavailable
	err      error
}

// ValidateStream checks every indexed AU against the same absolute byte
// stream. A bad AU is reported but never causes later offsets to be adjusted.
func ValidateStream(c Codec, stream []byte, indexes []timeline.Index) []Report {
	reports := make([]Report, 0, len(indexes))
	var lastOffset int64 = -1
	for i, idx := range indexes {
		rep := Report{
			Codec:           c,
			AUIndex:         i,
			Offset:          idx.Offset,
			Length:          idx.Length,
			ClaimedKeyframe: idx.Keyframe,
			FirstErrorByte:  -1,
		}
		if !c.Valid() {
			rep.fail(-1, timeline.New(timeline.KindArgument, "codec", i, idx.Offset, "unsupported codec %q", c))
			reports = append(reports, rep)
			continue
		}
		if idx.Offset < 0 || idx.Length < 0 || idx.Length > MaxAUBytes {
			at := idx.Offset
			if at < 0 {
				at = -1
			}
			rep.fail(at, timeline.New(timeline.KindUnrecoverable, "codec.index", i, at,
				"invalid offset/length offset=%d length=%d", idx.Offset, idx.Length))
			reports = append(reports, rep)
			continue
		}
		if idx.Offset < lastOffset {
			rep.fail(idx.Offset, timeline.New(timeline.KindUnrecoverable, "codec.index", i, idx.Offset,
				"offset moved backwards: %d < previous %d", idx.Offset, lastOffset))
			reports = append(reports, rep)
			continue
		}
		lastOffset = idx.Offset

		end := idx.Offset + int64(idx.Length)
		if idx.Offset > int64(len(stream)) || end > int64(len(stream)) {
			first := int64(len(stream))
			if idx.Offset > first {
				first = idx.Offset
			}
			rep.fail(first, timeline.New(timeline.KindUnrecoverable, "codec.truncated", i, first,
				"AU range [%d,%d) exceeds stream size %d", idx.Offset, end, len(stream)))
			reports = append(reports, rep)
			continue
		}

		au := stream[idx.Offset:end]
		info := parseBoundaries(c, au, idx.Keyframe)
		rep.Units = info.units
		rep.DetectedKeyframe = info.keyframe
		if info.err != nil {
			at := int64(-1)
			if info.errAt >= 0 {
				at = idx.Offset + int64(info.errAt)
			}
			rep.FirstErrorByte = at
			rep.Valid = false
			rep.Error = info.err.Error()
			var te *timeline.Error
			if errors.As(info.err, &te) {
				te.Index = i
				if at >= 0 {
					te.Offset = at
				}
				rep.Error = te.Error()
			} else {
				wrapped := timeline.Wrap(timeline.KindUnrecoverable, "codec.boundary", i, at, info.err)
				rep.Error = wrapped.Error()
			}
			reports = append(reports, rep)
			continue
		}
		rep.Valid = true
		reports = append(reports, rep)
	}
	return reports
}

func (r *Report) fail(at int64, err error) {
	r.Valid = false
	r.FirstErrorByte = at
	if te, ok := err.(*timeline.Error); ok {
		te.Offset = at
		r.Error = te.Error()
	} else {
		r.Error = err.Error()
	}
}

func parseBoundaries(c Codec, au []byte, claimedKey bool) boundaryInfo {
	switch c {
	case H264:
		return parseAnnexB(au, claimedKey, h264Spec)
	case H265:
		return parseAnnexB(au, claimedKey, h265Spec)
	case AV1:
		return parseAV1(au, claimedKey)
	default:
		return boundaryInfo{errAt: -1, err: fmt.Errorf("unsupported codec")}
	}
}

// ValidAUCount returns the number of valid reports.
func ValidAUCount(rs []Report) int {
	n := 0
	for _, r := range rs {
		if r.Valid {
			n++
		}
	}
	return n
}

// BadReports returns isolated reports.
func BadReports(rs []Report) []Report {
	var out []Report
	for _, r := range rs {
		if !r.Valid {
			out = append(out, r)
		}
	}
	return out
}
