// Package manifest aligns per-bitrate representation boundaries onto one
// canonical segment timeline and emits the shared init segment.
package manifest

import (
	"fmt"
	"io"

	"github.com/abema/go-mp4"
)

// Rep is one representation (bitrate ladder rung): the end frame of each
// of its segments, in order; the last entry is the total frame count.
type Rep struct {
	Bitrate    int
	Boundaries []int64
}

// RepOut is a representation aligned to the canonical timeline.
type RepOut struct {
	Bitrate    int     `json:"bitrate"`
	Boundaries []int64 `json:"boundaries"`
}

// Ladder is the aligned multi-bitrate manifest.
type Ladder struct {
	Timebase  string   `json:"timebase"`
	Canonical []int64  `json:"canonical"`
	Reps      []RepOut `json:"representations"`
}

// Align verifies every representation against the canonical timeline (the
// first representation): equal segment counts, strictly monotonic
// boundaries, and per-boundary deviation of at most maxDev frames. All
// representations are then snapped to the canonical boundaries so segment
// sequence numbers and the timebase stay monotonic and identical.
func Align(timebase string, reps []Rep, maxDev int64) (*Ladder, error) {
	if len(reps) == 0 {
		return nil, fmt.Errorf("manifest: no representations")
	}
	canon := reps[0].Boundaries
	if err := checkMonotonic(canon); err != nil {
		return nil, fmt.Errorf("manifest: reference rep: %w", err)
	}
	out := &Ladder{Timebase: timebase, Canonical: canon}
	for _, r := range reps {
		if len(r.Boundaries) != len(canon) {
			return nil, fmt.Errorf("manifest: rep %d: %d boundaries, want %d",
				r.Bitrate, len(r.Boundaries), len(canon))
		}
		if err := checkMonotonic(r.Boundaries); err != nil {
			return nil, fmt.Errorf("manifest: rep %d: %w", r.Bitrate, err)
		}
		for i, b := range r.Boundaries {
			if d := abs64(b - canon[i]); d > maxDev {
				return nil, fmt.Errorf("manifest: rep %d segment %d deviates %d frames (max %d)",
					r.Bitrate, i, d, maxDev)
			}
		}
		snapped := make([]int64, len(canon))
		copy(snapped, canon)
		out.Reps = append(out.Reps, RepOut{Bitrate: r.Bitrate, Boundaries: snapped})
	}
	return out, nil
}

func checkMonotonic(b []int64) error {
	for i := 1; i < len(b); i++ {
		if b[i] <= b[i-1] {
			return fmt.Errorf("boundaries not monotonic at index %d", i)
		}
	}
	return nil
}

// MarshalInit emits the shared fMP4 init segment (styp box) for the ladder.
func MarshalInit() ([]byte, error) {
	ws := &writeSeeker{}
	w := mp4.NewWriter(ws)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStyp()}); err != nil {
		return nil, fmt.Errorf("manifest: start styp: %w", err)
	}
	styp := &mp4.Styp{
		MajorBrand:   [4]byte{'m', 's', 'd', 'h'},
		MinorVersion: 0,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'m', 's', 'd', 'h'}},
			{CompatibleBrand: [4]byte{'m', 's', 'i', 'x'}},
		},
	}
	if _, err := mp4.Marshal(w, styp, mp4.Context{}); err != nil {
		return nil, fmt.Errorf("manifest: marshal styp: %w", err)
	}
	if _, err := w.EndBox(); err != nil {
		return nil, fmt.Errorf("manifest: end styp: %w", err)
	}
	return ws.buf, nil
}

// writeSeeker is an in-memory io.WriteSeeker for the mp4 writer.
type writeSeeker struct {
	buf []byte
	pos int64
}

func (w *writeSeeker) Write(p []byte) (int, error) {
	end := w.pos + int64(len(p))
	if grow := end - int64(len(w.buf)); grow > 0 {
		w.buf = append(w.buf, make([]byte, grow)...)
	}
	copy(w.buf[w.pos:end], p)
	w.pos = end
	return len(p), nil
}

func (w *writeSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = w.pos + offset
	case io.SeekEnd:
		abs = int64(len(w.buf)) + offset
	}
	if abs < 0 {
		return 0, fmt.Errorf("manifest: negative seek")
	}
	w.pos = abs
	return abs, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
