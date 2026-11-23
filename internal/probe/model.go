// Package probe scans MP4 and MXF containers into a shared, fault-tolerant
// metadata model: track time bases, sample tables, key frames, rotation,
// crop and colour descriptions. All timestamps are normalised to the
// internal/clock nanosecond time base.
package probe

import (
	"mediaroute/internal/clock"
	"mediaroute/internal/fault"
)

// ScannerVersion is embedded in every index so that a change of scanning
// logic invalidates all previously generated indexes.
const ScannerVersion = "mediaroute-probe-1"

// Hard limits guarding allocations on untrusted local input.
const (
	// MaxFileSize rejects containers above 512 MiB before scanning.
	MaxFileSize int64 = 512 << 20
	// MaxSamples bounds sample-table allocations.
	MaxSamples = 8 << 20
	// MaxTracks bounds per-container track allocations.
	MaxTracks = 128
)

// Container format identifiers.
const (
	FormatMP4 = "mp4"
	FormatMXF = "mxf"
)

// Colour triple (ISO/IEC 23001-8 / ITU-T H.273 indices).
type Colour struct {
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	FullRange bool
}

// DefaultColour is the fallback when neither track nor container declares
// a colour description: BT.709 SDR, limited range.
var DefaultColour = Colour{Primaries: 1, Transfer: 1, Matrix: 1}

// CropRect is a clean-aperture rectangle expressed in coded pixels.
type CropRect struct {
	Left   uint32
	Right  uint32
	Top    uint32
	Bottom uint32
}

// ColourScope records where an adopted colour description came from.
type ColourScope string

const (
	ScopeTrack     ColourScope = "track"
	ScopeContainer ColourScope = "container"
	ScopeDefault   ColourScope = "default"
)

// ColourDecision is the arbitration result for one track.
type ColourDecision struct {
	TrackID  uint32
	Adopted  Colour
	Source   ColourScope
	Track    *Colour // nil when absent
	Container *Colour // nil when absent
	Conflict bool
}

// TrackMeta describes one track and its fully expanded sample table.
type TrackMeta struct {
	TrackID   uint32
	Handler   string // "vide", "soun"
	Codec     string // fourCC e.g. "avc1","hvc1","mp4a"; "unknown" when unsupported
	Supported bool
	TimeBase  clock.Rational
	Width     uint32
	Height    uint32
	// Rotation is the normalised right-angle rotation in degrees.
	Rotation int
	// Crop is nil when no clean aperture is declared.
	Crop *CropRect
	// Colour decision after track/container/default arbitration.
	Colour ColourDecision
	// Samples are in decode order, indexed 0..len-1.
	Samples []SampleInfo
}

// SampleInfo is one fully resolved sample entry.
type SampleInfo struct {
	Index        int64
	Offset       int64 // byte offset of sample data inside the container
	Size         int64
	DTSTicks     int64 // decode timestamp in track time-base units
	DTSNanos     int64 // decode timestamp on the shared nanosecond base
	DurationTicks int64
	DurationNanos int64
	IsKeyframe    bool
}

// DurationNanos returns the track duration on the shared time base.
func (t *TrackMeta) DurationNanos() int64 {
	if len(t.Samples) == 0 {
		return 0
	}
	last := t.Samples[len(t.Samples)-1]
	return last.DTSNanos + last.DurationNanos
}

// KeyframeStats summarises key frame spacing in milliseconds.
type KeyframeStats struct {
	KeyframeCount int
	IntervalsMS   []int64 // gaps between consecutive key frames
	P50MS         int64
	P90MS         int64
	P95MS         int64
	P99MS         int64
	MaxMS         int64
}

// ConflictRecord documents a metadata conflict and its resolution.
type ConflictRecord struct {
	TrackID uint32
	Kind    string // "rotation","crop","colour"
	Detail  string
	Adopted string
}

// ContainerReport is the complete scan result fed to the index.
type ContainerReport struct {
	Format      string
	Path        string
	InputSHA256 string
	SizeBytes   int64
	Tracks      []TrackMeta
	KeyStats    map[uint32]KeyframeStats
	Conflicts   []ConflictRecord
	// Faults holds non-fatal structured faults (e.g. unsupported codec).
	Faults []*fault.Error
}

// TotalSamples sums sample counts across all tracks.
func (r *ContainerReport) TotalSamples() int {
	n := 0
	for i := range r.Tracks {
		n += len(r.Tracks[i].Samples)
	}
	return n
}
