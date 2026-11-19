// Package mp4 builds sample and keyframe indexes from MP4 files using go-mp4.
package mp4

import (
	"fmt"
	"io"
	"sort"

	"github.com/abema/go-mp4"

	"mediasync/internal/media"
)

// Limits bound indexing work.
const (
	MaxSamples = 2_000_000
	MaxBoxes   = 100_000
)

// Sample is one indexed media sample mapped onto a nanosecond timeline.
type Sample struct {
	Number     uint32 `json:"number"`
	Offset     int64  `json:"offset"`
	Size       uint32 `json:"size"`
	DTSNS      int64  `json:"dts_ns"`
	DurationNS int64  `json:"duration_ns"`
	Keyframe   bool   `json:"keyframe"`
}

// EndNS returns the sample end timestamp.
func (s Sample) EndNS() int64 { return s.DTSNS + s.DurationNS }

// Track is the full index of one media track.
type Track struct {
	TrackID     uint32         `json:"track_id"`
	Handler     string         `json:"handler"`
	Timescale   uint32         `json:"timescale"`
	DurationNS  int64          `json:"duration_ns"`
	Samples     []Sample       `json:"-"`
	Keyframes   []int          `json:"keyframe_indices"`
	Issues      []*media.Issue `json:"issues"`
	RebuiltKeys bool           `json:"rebuilt_keyframes"`
	File        string         `json:"file"`
}

// Fatal reports whether indexing produced an unrecoverable issue.
func (t *Track) Fatal() bool { return media.FirstFatal(t.Issues) != nil }

// SampleForTime returns the index of the sample covering ns, or -1.
func (t *Track) SampleForTime(ns int64) int {
	i := sort.Search(len(t.Samples), func(i int) bool { return t.Samples[i].EndNS() > ns })
	if i >= len(t.Samples) || t.Samples[i].DTSNS > ns {
		return -1
	}
	return i
}

// KeyframeAtOrBefore returns the index of the latest keyframe starting at or
// before ns, or -1 when none exists.
func (t *Track) KeyframeAtOrBefore(ns int64) int {
	best := -1
	for _, idx := range t.Keyframes {
		if t.Samples[idx].DTSNS <= ns {
			best = idx
		} else {
			break
		}
	}
	return best
}

// KeyframeAt returns the keyframe index whose DTS equals ns exactly, or -1.
func (t *Track) KeyframeAt(ns int64) int {
	for _, idx := range t.Keyframes {
		if t.Samples[idx].DTSNS == ns {
			return idx
		}
	}
	return -1
}

func (t *Track) issue(code media.Code, off int64, rec int, format string, args ...any) {
	t.Issues = append(t.Issues, media.New(media.StageMP4, media.InputMP4, code, format, args...).
		WithFile(t.File).WithOffset(off).WithRecord(rec))
}

func fatalErr(file string, format string, args ...any) *media.Issue {
	return media.New(media.StageMP4, media.InputMP4, media.CodeMalformed, format, args...).
		WithFile(file).WithOffset(0).AsFatal()
}

// translate converts media-timescale ticks to nanoseconds half-up.
func translate(ticks int64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	return int64((uint64(ticks)*1_000_000_000 + uint64(timescale)/2) / uint64(timescale))
}

func describe(err error) string {
	if err == nil {
		return ""
	}
	if ie, ok := err.(*media.Issue); ok {
		return ie.Message
	}
	return fmt.Sprint(err)
}

// seekable is the subset of go-mp4 inputs we rely on.
type seekable interface {
	io.ReadSeeker
}
