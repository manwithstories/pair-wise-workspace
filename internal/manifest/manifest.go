// Package manifest renders the delivery manifest: mapping, resolved
// metadata, per-track sample integrity and every fault location the run
// encountered. The manifest is plain text so it can be diffed and archived
// next to the output.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"mediacore/internal/adapter"
	"mediacore/internal/media"
	"mediacore/internal/metadata"
	"mediacore/internal/mux"
)

// Fault is a diagnosed problem preserved in the manifest.
type Fault struct {
	Class     string `json:"class"`
	Container string `json:"container,omitempty"`
	Track     uint32 `json:"track,omitempty"`
	Offset    int64  `json:"offset"`
	Message   string `json:"message"`
	Stage     string `json:"stage"` // parse, pipeline, mux, verify
	Tolerance int64  `json:"tolerance_bytes"`
}

// TrackEntry is one delivered track.
type TrackEntry struct {
	Key          string   `json:"key"`
	OutputID     uint32   `json:"output_id"`
	Type         string   `json:"type"`
	Codec        string   `json:"codec"`
	Samples      int      `json:"samples"`
	InputSHA256  string   `json:"input_sha256"`
	OutputSHA256 string   `json:"output_sha256"`
	Match        bool     `json:"sha256_match"`
	Rotation     int      `json:"rotation"`
	RotationFrom string   `json:"rotation_from"`
	Crop         string   `json:"crop"`
	Color        string   `json:"color"`
	ColorFrom    string   `json:"color_from"`
	Warnings     []string `json:"warnings,omitempty"`
	Conflicts    []string `json:"conflicts,omitempty"`
}

// Manifest is the complete delivery record.
type Manifest struct {
	Inputs           []string     `json:"inputs"`
	Adapter          string       `json:"adapter"`
	Version          string       `json:"adapter_version"`
	Fingerprint      string       `json:"input_fingerprint"`
	Output           string       `json:"output"`
	OutputSize       int64        `json:"output_size"`
	TotalSamples     int          `json:"total_samples"`
	Tracks           []TrackEntry `json:"tracks"`
	Faults           []Fault      `json:"faults,omitempty"`
	UnknownBoxes     int          `json:"unknown_boxes"`
	QueuePeak        int64        `json:"max_queue_bytes"`
	Resume           *ResumeInfo  `json:"resume,omitempty"`
	RemuxSHA256Match bool         `json:"remux_sha256_match"`
}

// ResumeInfo records resume behaviour for the delivery record.
type ResumeInfo struct {
	ResumedReplaySamples int   `json:"replay_samples"`
	VerifiedSegments     int   `json:"verified_segments"`
	RolledBackSegments   int   `json:"rolled_back_segments"`
	ResumeMillis         int64 `json:"resume_ms"`
}

// Build assembles a manifest from the pieces of a run.
func Build(in Input) *Manifest {
	m := &Manifest{
		Inputs:       in.Inputs,
		Adapter:      in.Adapter,
		Version:      in.Version,
		Fingerprint:  in.Fingerprint,
		Output:       in.Output,
		OutputSize:   in.OutputSize,
		UnknownBoxes: in.UnknownBoxes,
		QueuePeak:    in.QueuePeak,
		Resume:       in.Resume,
	}
	if in.Remux != nil {
		m.TotalSamples = in.Remux.Samples
	}
	if in.TotalSamples > 0 {
		m.TotalSamples = in.TotalSamples
	}
	match := true
	for _, tr := range in.RemuxTracks {
		e := TrackEntry{
			Key:          tr.Key,
			OutputID:     tr.OutputID,
			Samples:      tr.Samples,
			InputSHA256:  tr.InputSHA256,
			OutputSHA256: tr.OutputSHA256,
			Match:        tr.Match(),
		}
		if !e.Match {
			match = false
		}
		if r, ok := lookupResolved(in.Resolved, tr.Key); ok {
			e.Type = string(in.TrackTypes[tr.Key])
			e.Codec = in.TrackCodecs[tr.Key]
			e.Rotation = r.Rotation
			e.RotationFrom = r.RotationFrom
			if r.Crop != nil {
				e.Crop = r.Crop.String()
			}
			e.Color = r.Color.String()
			e.ColorFrom = r.ColorFrom
			e.Warnings = r.Warnings
			e.Conflicts = append(append([]string(nil), r.RotationConflicts...), r.ColorConflicts...)
		}
		m.Tracks = append(m.Tracks, e)
	}
	sort.SliceStable(m.Tracks, func(i, j int) bool { return m.Tracks[i].OutputID < m.Tracks[j].OutputID })
	m.Faults = in.Faults
	m.RemuxSHA256Match = match && len(m.Tracks) > 0
	return m
}

// Input is everything Build needs.
type Input struct {
	Inputs       []string
	Adapter      string
	Version      string
	Fingerprint  string
	Output       string
	OutputSize   int64
	TotalSamples int
	UnknownBoxes int
	QueuePeak    int64
	Resume       *ResumeInfo
	Remux        *mux.Result
	RemuxTracks  []mux.TrackResult
	Resolved     *metadata.Result
	TrackTypes   map[string]media.MediaType
	TrackCodecs  map[string]string
	Faults       []Fault
}

func lookupResolved(r *metadata.Result, key string) (metadata.Resolved, bool) {
	if r == nil {
		return metadata.Resolved{}, false
	}
	return r.Lookup(key)
}

// FaultFromError converts a classified error into a manifest fault.
func FaultFromError(stage string, err error) *Fault {
	if err == nil {
		return nil
	}
	f := &Fault{
		Class:     string(media.ClassOf(err)),
		Stage:     stage,
		Offset:    media.OffsetOf(err),
		Message:   err.Error(),
		Tolerance: 0,
	}
	if e, ok := err.(*media.Error); ok {
		f.Container = e.Container
		if e.HasTrack {
			f.Track = e.Track
		}
		f.Tolerance = e.Tolerance
	}
	return f
}

// WriteJSON emits the manifest as indented JSON.
func (m *Manifest) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// Summary renders the one-line delivery summary used by the CLI and tests.
func (m *Manifest) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "tracks=%d samples=%d max_queue_bytes=%d remux_sha256_match=%t",
		len(m.Tracks), m.TotalSamples, m.QueuePeak, m.RemuxSHA256Match)
	return b.String()
}

// PlanText renders the mapping and resolved metadata for terminal review.
func (m *Manifest) PlanText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "adapter=%s/%s fingerprint=%s\n", m.Adapter, m.Version, short(m.Fingerprint))
	for _, t := range m.Tracks {
		fmt.Fprintf(&b, "  out=%d key=%s type=%s codec=%s samples=%d rotation=%d(%s) crop=%s color=%s(%s)\n",
			t.OutputID, t.Key, t.Type, t.Codec, t.Samples, t.Rotation, t.RotationFrom, t.Crop, t.Color, t.ColorFrom)
		for _, c := range t.Conflicts {
			fmt.Fprintf(&b, "    conflict: %s\n", c)
		}
		for _, w := range t.Warnings {
			fmt.Fprintf(&b, "    warning: %s\n", w)
		}
	}
	for _, f := range m.Faults {
		fmt.Fprintf(&b, "  fault stage=%s class=%s container=%s track=%d offset=%d tolerance_bytes=%d: %s\n",
			f.Stage, f.Class, f.Container, f.Track, f.Offset, f.Tolerance, f.Message)
	}
	return b.String()
}

func short(fp string) string {
	if len(fp) > 16 {
		return fp[:16]
	}
	return fp
}

// MappingJSON is a convenience for callers that only need the plan.
func MappingJSON(plan *adapter.Plan) ([]byte, error) {
	return json.MarshalIndent(plan, "", "  ")
}
