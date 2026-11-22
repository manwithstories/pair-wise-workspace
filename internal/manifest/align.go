// Package manifest generates an archival manifest and checks it against the
// repaired timeline.  Track start/end boundaries must agree within one
// millisecond; a larger error is a verification failure carrying the error
// in milliseconds and the associated block offsets.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
	"archivecheck/internal/timeline"
)

// Tolerance is the maximum permitted alignment error (1 ms inclusive).
var Tolerance = clock.MustNew(1, 1000)

// TrackRange is one track's expected and observed extent.
type TrackRange struct {
	Track             uint64 `json:"track"`
	Kind              string `json:"kind"`
	DeclaredStartUsec int64  `json:"declared_start_us"`
	DeclaredEndUsec   int64  `json:"declared_end_us"`
	ObservedStartUsec int64  `json:"observed_start_us"`
	ObservedEndUsec   int64  `json:"observed_end_us"`
	StartErrorUsec    int64  `json:"start_error_us"`
	EndErrorUsec      int64  `json:"end_error_us"`
	StartOffset       int64  `json:"start_block_offset"`
	EndOffset         int64  `json:"end_block_offset"`
}

// Manifest is the archival checklist written beside the index.
type Manifest struct {
	Version      int          `json:"version"`
	DurationUsec int64        `json:"duration_us"`
	Tracks       []TrackRange `json:"tracks"`
	IndexDigest  string       `json:"index_sha256"`
	FrameCount   int          `json:"frame_count"`
}

// Alignment is the verification outcome.
type Alignment struct {
	MaxErrorUsec int64        `json:"manifest_alignment_us"`
	MaxErrorMs   float64      `json:"manifest_alignment_ms"`
	PerTrack     []TrackRange `json:"-"`
	Faults       *fault.Set
	OK           bool
}

// Declared is an archive checklist's expected extent for one track, derived
// from the source's intended timeline rather than the repaired one.
type Declared struct {
	Track      uint64
	Kind       string
	Start, End clock.Rational
}

// Generate builds a manifest from the repaired tracks and the independently
// declared extent of each track.  Declared times come from the archive's
// intended timeline; observed times are measured from the repaired frames, so
// verification genuinely compares two derivations.
func Generate(tracks map[uint64]*timeline.TrackInfo, declared []Declared, indexDigest string) Manifest {
	m := Manifest{Version: 1, IndexDigest: indexDigest}
	byTrack := make(map[uint64]Declared, len(declared))
	for _, d := range declared {
		byTrack[d.Track] = d
	}
	var maxEnd clock.Rational
	nums := make([]uint64, 0, len(declared))
	for _, d := range declared {
		nums = append(nums, d.Track)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	for _, n := range nums {
		d := byTrack[n]
		info := tracks[n]
		if info == nil {
			m.Tracks = append(m.Tracks, TrackRange{
				Track: n, Kind: d.Kind,
				DeclaredStartUsec: toMicros(d.Start), DeclaredEndUsec: toMicros(d.End),
				StartOffset: -1, EndOffset: -1,
			})
			continue
		}
		start, end := observedExtent(info)
		m.Tracks = append(m.Tracks, TrackRange{
			Track: n, Kind: d.Kind,
			DeclaredStartUsec: toMicros(d.Start), DeclaredEndUsec: toMicros(d.End),
			ObservedStartUsec: toMicros(start), ObservedEndUsec: toMicros(end),
			StartOffset: firstOffset(info), EndOffset: lastOffset(info),
		})
		m.FrameCount += info.Visible()
		if d.End.Cmp(maxEnd) > 0 {
			maxEnd = d.End
		}
	}
	m.DurationUsec = toMicros(maxEnd)
	return m
}

// observedExtent measures a track's repaired extent: first visible timestamp
// to last visible timestamp plus one default frame duration, matching how the
// container declares its end boundary.
func observedExtent(info *timeline.TrackInfo) (clock.Rational, clock.Rational) {
	start, end := info.StartEnd()
	if info.DefaultNS > 0 {
		end = end.Add(clock.MustNew(int64(info.DefaultNS), 1_000_000_000))
	}
	return start, end
}

func firstOffset(t *timeline.TrackInfo) int64 {
	best := int64(-1)
	for _, f := range t.Frames {
		if f.Missing {
			continue
		}
		if best < 0 || f.Offset < best {
			best = f.Offset
		}
	}
	return best
}

func lastOffset(t *timeline.TrackInfo) int64 {
	best := int64(-1)
	for _, f := range t.Frames {
		if f.Missing {
			continue
		}
		if f.Offset > best {
			best = f.Offset
		}
	}
	return best
}

func toMicros(r clock.Rational) int64 { return int64(r.Float64() * 1e6) }

// Verify recomputes each track's observed extent from the repaired timeline
// and compares it with the manifest.  An error beyond Tolerance marks the
// manifest as failed and records the offending block offsets.
func Verify(m Manifest, tracks map[uint64]*timeline.TrackInfo) Alignment {
	al := Alignment{Faults: &fault.Set{}, OK: true}
	for _, declared := range m.Tracks {
		info := tracks[declared.Track]
		if info == nil {
			al.OK = false
			al.Faults.Add(fault.New(fault.ModuleManifest, fault.KindManifestDrift, fault.NoOffset(),
				fmt.Sprintf("manifest lists track %d but timeline lacks it", declared.Track)).OnTrack(declared.Track))
			continue
		}
		start, end := observedExtent(info)
		gotStart, gotEnd := toMicros(start), toMicros(end)
		tr := declared
		tr.ObservedStartUsec = gotStart
		tr.ObservedEndUsec = gotEnd
		tr.StartErrorUsec = gotStart - declared.DeclaredStartUsec
		tr.EndErrorUsec = gotEnd - declared.DeclaredEndUsec
		al.PerTrack = append(al.PerTrack, tr)

		for _, err := range []struct {
			name   string
			usec   int64
			offset int64
		}{
			{"start", tr.StartErrorUsec, declared.StartOffset},
			{"end", tr.EndErrorUsec, declared.EndOffset},
		} {
			al.errAbs(err.usec)
			if absMicros(err.usec) > toMicros(Tolerance) {
				al.OK = false
				off := fault.NoOffset()
				if err.offset >= 0 {
					off = fault.NewOffset(uint64(err.offset))
				}
				fd := fault.New(fault.ModuleManifest, fault.KindManifestDrift, off,
					fmt.Sprintf("track %d %s boundary is off by %.3f ms (limit %.3f ms); manifest not committed; associated block offset 0x%x",
						declared.Track, err.name, float64(err.usec)/1000.0, Tolerance.Millis(), err.offset)).OnTrack(declared.Track)
				al.Faults.Add(fd)
			}
		}
	}
	al.MaxErrorMs = float64(al.MaxErrorUsec) / 1000.0
	return al
}

func (al *Alignment) errAbs(usec int64) {
	if a := absMicros(usec); a > al.MaxErrorUsec {
		al.MaxErrorUsec = a
	}
}

func absMicros(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// WriteJSON serializes the manifest deterministically.
func (m Manifest) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// ToleranceMillis returns the alignment tolerance expressed in milliseconds.
func ToleranceMillis() float64 { return Tolerance.Millis() }
