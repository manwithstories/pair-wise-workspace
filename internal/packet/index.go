package packet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediaroute/internal/clock"
	"mediaroute/internal/manifest"
)

// MaxIndexEntries bounds the random-access index size.
const MaxIndexEntries = 1 << 16

// Entry is one keyframe anchor in the rebuilt index.
type Entry struct {
	Segment int    `json:"segment"`
	Ticks   uint64 `json:"ticks"`
	Bytes   int64  `json:"bytes"`
}

// Builder collects keyframe entries in segment order.
type Builder struct {
	entries []Entry
}

// NewBuilder creates an empty index builder.
func NewBuilder() *Builder { return &Builder{} }

// AddKeyframe records a keyframe. Segments must be added in increasing
// order; truncated packets must never reach this function.
func (b *Builder) AddKeyframe(seg int, ticks uint64, bytes int64) error {
	if len(b.entries) >= MaxIndexEntries {
		return fmt.Errorf("packet: index exceeds %d entries", MaxIndexEntries)
	}
	if n := len(b.entries); n > 0 && b.entries[n-1].Segment >= seg {
		return fmt.Errorf("packet: index segment order violation at %d", seg)
	}
	b.entries = append(b.entries, Entry{Segment: seg, Ticks: ticks, Bytes: bytes})
	return nil
}

// Entries returns the collected entries.
func (b *Builder) Entries() []Entry { return b.entries }

// WriteAtomic serialises the index to path via a temporary file and rename,
// so a crash never leaves a partial index behind.
func (b *Builder) WriteAtomic(path string) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, e := range b.entries {
		if err := enc.Encode(e); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Clean(path))
}

// Verify compares index entry times against manifest intervals on the
// unified 90 kHz base. Any entry off by more than frameTicks is a failure.
// It returns the mean and worst error in milliseconds plus failure records.
func Verify(entries []Entry, intervals []manifest.Interval, frameTicks uint64) (meanMs, maxMs float64, failures []string) {
	bySeq := make(map[int]manifest.Interval, len(intervals))
	for _, iv := range intervals {
		bySeq[iv.Seq] = iv
	}
	var sum float64
	for _, e := range entries {
		iv, ok := bySeq[e.Segment]
		if !ok {
			failures = append(failures, fmt.Sprintf("segment %d: no manifest interval", e.Segment))
			continue
		}
		var d uint64
		if e.Ticks > iv.Start {
			d = e.Ticks - iv.Start
		} else {
			d = iv.Start - e.Ticks
		}
		ms := clock.TicksToMillisFloat(d)
		sum += ms
		if ms > maxMs {
			maxMs = ms
		}
		if d > frameTicks {
			failures = append(failures, fmt.Sprintf(
				"segment %d: index time %.3f ms off manifest interval [%d,%d) exceeds one frame",
				e.Segment, clock.TicksToMillisFloat(e.Ticks), iv.Start, iv.End))
		}
	}
	if len(entries) > 0 {
		meanMs = sum / float64(len(entries))
	}
	return meanMs, maxMs, failures
}
