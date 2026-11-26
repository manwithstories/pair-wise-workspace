// Package index rebuilds sample offsets, sync sample tables, and verified
// random-access points.
package index

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"mediad/internal/media"
)

// SyncMagic is the fixture-independent marker used to prove a seek landed on
// the intended sync sample without invoking a media decoder.
const SyncMagic = "MDIASYNC!"

// SeekPoint is one random-access location.
type SeekPoint struct {
	TrackID      uint32 `json:"track_id"`
	SampleNumber uint32 `json:"sample_number"`
	TimeNS       int64  `json:"time_ns"`
	Offset       int64  `json:"offset"`
	Size         uint32 `json:"size"`
	Verified     bool   `json:"verified"`
	Check        string `json:"check,omitempty"`
}

// TrackIndex contains sorted random access points for one track.
type TrackIndex struct {
	TrackID uint32      `json:"track_id"`
	Points  []SeekPoint `json:"points"`
}

// Index is the publishable random-access index.
type Index struct {
	Complete bool                   `json:"complete"`
	Tracks   map[uint32]*TrackIndex `json:"tracks"`
	Misses   int                    `json:"seek_misses"`
	Faults   []*media.Fault         `json:"faults,omitempty"`
}

// Rebuild constructs an in-memory index from normalized tracks. Call Verify
// before publishing it.
func Rebuild(p *media.Project) (*Index, []*media.Fault) {
	idx := &Index{Complete: false, Tracks: map[uint32]*TrackIndex{}}
	var faults []*media.Fault
	for _, t := range p.Tracks {
		if t == nil {
			continue
		}
		clock, err := t.Clock()
		if err != nil {
			f := media.NewFault(media.CodeInvalidInput, "%v", err).WithTrack(t.ID).Fatal()
			faults = append(faults, f)
			continue
		}
		ti := &TrackIndex{TrackID: t.ID}
		for _, s := range t.Samples {
			if !s.Sync {
				continue
			}
			ns, err := clock.TicksToNanoseconds(s.PTS)
			if err != nil {
				f := media.NewFault(media.CodeInvalidInput, "index time overflow sample %d: %v", s.Index, err).WithTrack(t.ID).Fatal()
				faults = append(faults, f)
				continue
			}
			if s.Offset < 0 {
				f := media.NewFault(media.CodeBadSampleOffset, "sync sample %d has no offset", s.Index).
					WithTrack(t.ID).WithBox("stco").Fatal()
				faults = append(faults, f)
				continue
			}
			ti.Points = append(ti.Points, SeekPoint{
				TrackID: t.ID, SampleNumber: s.Index, TimeNS: ns,
				Offset: s.Offset, Size: s.Size,
			})
		}
		sort.Slice(ti.Points, func(i, j int) bool { return ti.Points[i].SampleNumber < ti.Points[j].SampleNumber })
		if len(ti.Points) == 0 {
			f := media.NewFault(media.CodeMissingIndex, "track has no sync samples").WithTrack(t.ID).WithBox("stss").Fatal()
			faults = append(faults, f)
		}
		idx.Tracks[t.ID] = ti
	}
	if len(faults) == 0 {
		idx.Complete = true
	}
	idx.Faults = faults
	return idx, faults
}

// Verify reads the first bytes at every seek point and checks the sync marker.
// It is streaming in the sense that it reads only fixed-width prefixes.
func (idx *Index) Verify(r io.ReadSeeker, fileSize int64) (int, []*media.Fault) {
	if idx == nil {
		return 0, []*media.Fault{media.NewFault(media.CodeIndexIncomplete, "nil index").Fatal()}
	}
	var faults []*media.Fault
	misses := 0
	for _, ti := range idx.Tracks {
		for i := range ti.Points {
			pt := &ti.Points[i]
			fail := func(format string, args ...any) {
				misses++
				pt.Verified = false
				pt.Check = fmt.Sprintf(format, args...)
				f := media.NewFault(media.CodeIndexIncomplete, "%s", pt.Check).
					WithTrack(pt.TrackID).WithByteRange(pt.Offset, int64(minU32(pt.Size, uint32(len(SyncMagic)))))
				faults = append(faults, f)
			}
			if pt.Offset < 0 || int64(pt.Size) < int64(len(SyncMagic)) || pt.Offset+int64(pt.Size) > fileSize {
				fail("seek point sample %d interval invalid: offset=%d size=%d file=%d", pt.SampleNumber, pt.Offset, pt.Size, fileSize)
				continue
			}
			if _, err := r.Seek(pt.Offset, io.SeekStart); err != nil {
				fail("seek failed: %v", err)
				continue
			}
			magic := make([]byte, len(SyncMagic))
			if _, err := io.ReadFull(r, magic); err != nil {
				fail("read sync marker: %v", err)
				continue
			}
			if string(magic) != SyncMagic {
				fail("sync marker mismatch at byte %d", pt.Offset)
				continue
			}
			pt.Verified = true
			pt.Check = "sync-sample"
		}
	}
	idx.Misses = misses
	idx.Faults = append(idx.Faults, faults...)
	if misses > 0 {
		idx.Complete = false
	}
	return misses, faults
}

// NearestSync returns the last verified sync point at or before sampleNumber.
func (idx *Index) NearestSync(trackID, sampleNumber uint32) (SeekPoint, bool) {
	ti := idx.Tracks[trackID]
	if ti == nil {
		return SeekPoint{}, false
	}
	var best *SeekPoint
	for i := range ti.Points {
		p := &ti.Points[i]
		if p.SampleNumber <= sampleNumber && p.Verified {
			best = p
		}
	}
	if best == nil {
		return SeekPoint{}, false
	}
	return *best, true
}

// WriteJSON publishes an index only when every point was verified.
func (idx *Index) WriteJSON(path string) error {
	if idx == nil || !idx.Complete || idx.Misses != 0 {
		return media.NewFault(media.CodeIndexIncomplete, "refusing to publish incomplete index").Fatal()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func minU32(a uint32, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}
