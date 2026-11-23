// Package index builds a random-access index over repaired samples and
// encodes resumable checkpoints of that index.
package index

import (
	"fmt"
	"sort"

	"archivecheck/internal/fault"
	"archivecheck/internal/repair"
)

// Entry indexes one sample by decode time and byte range.
type Entry struct {
	Segment    string
	Sequence   uint32
	TrackID    uint32
	Timescale  uint32
	DTS        uint64
	EndDTS     uint64
	Offset     int64
	Size       uint32
	Sync       bool
	Seekable   bool
	IntervalID int
}

// Interval groups samples between sync points. Intervals without a usable
// sync sample are explicitly not seekable.
type Interval struct {
	ID        int    `json:"id"`
	TrackID   uint32 `json:"track_id"`
	Timescale uint32 `json:"timescale"`
	StartDTS  uint64 `json:"start_dts"`
	EndDTS    uint64 `json:"end_dts"`
	Role      string `json:"role"`
	Seekable  bool   `json:"seekable"`
	// SyncDTS is the decode time of the sync sample starting the interval.
	SyncDTS uint64 `json:"sync_dts"`
}

// Index is the full random-access index for an archive.
type Index struct {
	Entries    []Entry        `json:"entries"`
	Intervals  []Interval     `json:"intervals"`
	Unseekable int            `json:"unseekable"`
	ByTrack    map[uint32]int `json:"by_track"`
}

// Build flattens a repaired timeline into a normalized index.
func Build(tl *repair.Timeline) (*Index, error) {
	if tl == nil {
		return nil, fault.New(fault.KindIndex, "", "trun", 0, "nil timeline")
	}
	var entries []Entry
	for _, ft := range tl.Fragments {
		if ft.Timescale == 0 {
			return nil, fault.New(fault.KindIndex, ft.Segment, "mdhd", 0,
				fmt.Sprintf("track %d has zero timescale in index input", ft.TrackID))
		}
		for _, s := range ft.Samples {
			if s.EndDTS <= s.DTS {
				return nil, fault.New(fault.KindIndex, ft.Segment, "trun", s.Offset,
					fmt.Sprintf("sample has non-positive duration: dts=%d end=%d", s.DTS, s.EndDTS))
			}
			if s.Offset < 0 || s.Size == 0 {
				return nil, fault.New(fault.KindIndex, ft.Segment, "mdat", s.Offset,
					fmt.Sprintf("sample range invalid: offset=%d size=%d", s.Offset, s.Size))
			}
			entries = append(entries, Entry{
				Segment: ft.Segment, Sequence: ft.Sequence, TrackID: ft.TrackID,
				Timescale: ft.Timescale, DTS: s.DTS, EndDTS: s.EndDTS,
				Offset: s.Offset, Size: s.Size, Sync: s.Sync(),
			})
		}
	}
	return Normalize(entries)
}

// Normalize sorts entries deterministically (track, DTS, output offset) and
// assigns sync-delimited intervals. Intervals that do not begin with a sync
// sample are marked unseekable.
func Normalize(entries []Entry) (*Index, error) {
	idx := &Index{ByTrack: map[uint32]int{}}
	idx.Entries = append(idx.Entries, entries...)
	sort.SliceStable(idx.Entries, func(a, b int) bool {
		x, y := idx.Entries[a], idx.Entries[b]
		if x.TrackID != y.TrackID {
			return x.TrackID < y.TrackID
		}
		if x.DTS != y.DTS {
			return x.DTS < y.DTS
		}
		return x.Offset < y.Offset
	})
	active := map[uint32]int{}
	intervalByID := map[int]*Interval{}
	nextID := 0
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if e.EndDTS <= e.DTS {
			return nil, fault.New(fault.KindIndex, e.Segment, "trun", e.Offset,
				fmt.Sprintf("entry %d has non-positive duration", i))
		}
		if e.Timescale == 0 {
			return nil, fault.New(fault.KindIndex, e.Segment, "mdhd", 0, "entry has zero timescale")
		}
		current, open := active[e.TrackID]
		if !open || e.Sync {
			current = nextID
			nextID++
			active[e.TrackID] = current
			iv := Interval{
				ID: current, TrackID: e.TrackID, Timescale: e.Timescale,
				StartDTS: e.DTS, EndDTS: e.EndDTS, SyncDTS: e.DTS,
				Seekable: e.Sync, Role: intervalRole(e.Sync),
			}
			intervalByID[current] = &iv
			idx.Intervals = append(idx.Intervals, iv)
		}
		iv := intervalByID[current]
		if e.EndDTS > iv.EndDTS {
			iv.EndDTS = e.EndDTS
		}
		e.IntervalID = iv.ID
		e.Seekable = iv.Seekable
		idx.ByTrack[e.TrackID]++
	}
	// Copy interval end times back from the local map.
	for i := range idx.Intervals {
		idx.Intervals[i] = *intervalByID[idx.Intervals[i].ID]
		if !idx.Intervals[i].Seekable {
			idx.Unseekable++
		}
	}
	sort.SliceStable(idx.Intervals, func(a, b int) bool {
		x, y := idx.Intervals[a], idx.Intervals[b]
		if x.TrackID != y.TrackID {
			return x.TrackID < y.TrackID
		}
		if x.StartDTS != y.StartDTS {
			return x.StartDTS < y.StartDTS
		}
		return x.ID < y.ID
	})
	return idx, nil
}

func intervalRole(sync bool) string {
	if sync {
		return "sync"
	}
	return "non_sync"
}

// SeekPoints returns the first entry of every seekable interval, which is the
// minimal random-access set for the archive.
func (idx *Index) SeekPoints() []Entry {
	var out []Entry
	for _, iv := range idx.Intervals {
		if !iv.Seekable {
			continue
		}
		for _, e := range idx.Entries {
			if e.IntervalID == iv.ID {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// Validate checks internal consistency: monotonic per-track DTS, positive
// durations, and sample ranges inside the output file.
func (idx *Index) Validate(fileSize int64) error {
	if idx == nil {
		return fault.New(fault.KindIndex, "", "", 0, "nil index")
	}
	lastEnd := map[uint32]uint64{}
	for i, e := range idx.Entries {
		if e.EndDTS <= e.DTS {
			return fault.New(fault.KindIndex, e.Segment, "trun", e.Offset,
				fmt.Sprintf("entry %d has non-positive duration", i))
		}
		if prev, ok := lastEnd[e.TrackID]; ok && e.DTS < prev {
			return fault.New(fault.KindIndex, e.Segment, "trun", e.Offset,
				fmt.Sprintf("entry %d regresses track %d: dts=%d previous end=%d", i, e.TrackID, e.DTS, prev))
		}
		lastEnd[e.TrackID] = e.EndDTS
		if e.Offset < 0 || e.Size == 0 || int64(e.Size) > fileSize || e.Offset > fileSize-int64(e.Size) {
			return fault.New(fault.KindIndex, e.Segment, "mdat", e.Offset,
				fmt.Sprintf("entry %d sample range [%d,%d) outside file of %d bytes", i, e.Offset, e.Offset+int64(e.Size), fileSize))
		}
	}
	return nil
}
