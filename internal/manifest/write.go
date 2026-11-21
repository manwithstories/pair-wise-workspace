// Package manifest writes a delivery manifest and independently verifies that
// what the manifest claims matches what the output timeline actually contains.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"

	"mediameld/internal/mapping"
	"mediameld/internal/model"
	"mediameld/internal/remux"
	"mediameld/internal/timeline"
)

// Manifest is the on-disk record of a delivery.
type Manifest struct {
	Version      int            `json:"version"`
	Sources      []SourceRecord `json:"sources"`
	Tracks       []TrackRecord  `json:"tracks"`
	TimeRange    TimeRange      `json:"time_range"`
	Gaps         []GapRecord    `json:"gaps"`
	Metadata     []MetaDecision `json:"metadata"`
	Samples      []SampleRecord `json:"samples"`
	TimelineHash string         `json:"timeline_sha256"`
	SamplesCount int            `json:"samples_count"`
	LateReplays  int            `json:"late_replays"`
	Quality      Quality        `json:"quality"`
	Bitrate      Bitrate        `json:"bitrate"`
}

// Bitrate is the independent container-vs-media accounting.
type Bitrate struct {
	TargetBPS           int64   `json:"target_bps"`
	ActualBPS           int64   `json:"actual_bps"`
	DeviationPct        float64 `json:"deviation_pct"`
	ContainerBytes      int64   `json:"container_bytes"`
	MediaBytes          int64   `json:"media_bytes"`
	DeliveredDurationNS int64   `json:"delivered_duration_ns"`
}

type SourceRecord struct {
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
	Bytes       int64  `json:"bytes"`
}

type TrackRecord struct {
	ID          string `json:"id"`
	Role        string `json:"role"`
	Kind        string `json:"kind"`
	Fingerprint string `json:"fingerprint"`
	Timescale   int64  `json:"timescale"`
	Samples     int    `json:"samples"`
	Rotation    int    `json:"rotation"`
	Crop        string `json:"crop"`
	Color       string `json:"color"`
}

type TimeRange struct {
	StartNS int64 `json:"start_ns"`
	EndNS   int64 `json:"end_ns"`
}

type GapRecord struct {
	TrackID  string `json:"track_id"`
	StartNS  int64  `json:"start_ns"`
	EndNS    int64  `json:"end_ns"`
	Sequence uint32 `json:"sequence"`
}

type MetaDecision struct {
	TrackID  string `json:"track_id"`
	Property string `json:"property"`
	Source   string `json:"source"`
	Original string `json:"original"`
	Chosen   string `json:"chosen"`
	Why      string `json:"why"`
}

type SampleRecord struct {
	TrackID  string `json:"track_id"`
	Index    int    `json:"index"`
	PTSNS    int64  `json:"pts_ns"`
	Duration int64  `json:"duration_ns"`
	Hash     string `json:"sha256"`
	Size     int32  `json:"size"`
	Gap      bool   `json:"gap,omitempty"`
}

// Quality reports hash-based (not subjective) quality accounting.
type Quality struct {
	Matched      int     `json:"matched"`
	Total        int     `json:"total"`
	MatchRatePct float64 `json:"match_rate_pct"`
}

// Build assembles a manifest from the resolved tracks and the stable timeline.
func Build(plan timeline.Plan, resolved []mapping.Resolution, remuxRes remux.Result, sources []SourceRecord) Manifest {
	m := Manifest{
		Version:      1,
		Sources:      sources,
		Tracks:       []TrackRecord{},
		Gaps:         []GapRecord{},
		Metadata:     []MetaDecision{},
		Samples:      []SampleRecord{},
		TimelineHash: plan.Hash,
		SamplesCount: len(remuxRes.SampleHashes),
		LateReplays:  plan.LateReplays,
	}
	if m.Sources == nil {
		m.Sources = []SourceRecord{}
	}
	// Time range from the delivered samples.
	first := true
	for _, e := range plan.Events {
		if e.IsGap {
			continue
		}
		if first {
			m.TimeRange.StartNS = int64(e.PTS)
			m.TimeRange.EndNS = int64(e.PTS + e.Duration)
			first = false
			continue
		}
		if int64(e.PTS) < m.TimeRange.StartNS {
			m.TimeRange.StartNS = int64(e.PTS)
		}
		if int64(e.PTS+e.Duration) > m.TimeRange.EndNS {
			m.TimeRange.EndNS = int64(e.PTS + e.Duration)
		}
	}
	for _, e := range plan.Events {
		if e.IsGap {
			m.Gaps = append(m.Gaps, GapRecord{
				TrackID: e.TrackID, StartNS: int64(e.PTS), EndNS: int64(e.PTS + e.Duration), Sequence: e.Seq,
			})
			continue
		}
		m.Samples = append(m.Samples, SampleRecord{
			TrackID: e.TrackID, Index: e.Index, PTSNS: int64(e.PTS),
			Duration: int64(e.Duration), Hash: e.HashHex(), Size: e.Size, Gap: false,
		})
	}
	// Track table carries role, geometry and adjudicated metadata.
	decisionByTrack := map[string]mapping.DisplayDecision{}
	for _, r := range resolved {
		decisionByTrack[r.Track.ID] = r.Decision
	}
	for _, r := range resolved {
		d := r.Decision
		m.Tracks = append(m.Tracks, TrackRecord{
			ID: r.Track.ID, Role: string(r.Role), Kind: string(r.Track.Kind),
			Fingerprint: r.Fingerprint, Timescale: r.Track.Timescale,
			Samples: len(r.Track.Samples), Rotation: d.Rotation,
			Crop: cropString(d.Crop), Color: colorString(d.Color),
		})
		for _, reason := range d.Reasons {
			m.Metadata = append(m.Metadata, MetaDecision{
				TrackID: r.Track.ID, Property: reason.Field, Source: reason.Source,
				Original: reason.Original, Chosen: reason.Chosen, Why: reason.Why,
			})
		}
	}
	m.Bitrate = Bitrate{
		ContainerBytes:      remuxRes.Bytes,
		DeliveredDurationNS: m.TimeRange.EndNS - m.TimeRange.StartNS,
	}
	for _, s := range remuxRes.SampleHashes {
		m.Bitrate.MediaBytes += int64(s.Size)
	}
	if span := m.Bitrate.DeliveredDurationNS; span > 0 {
		m.Bitrate.TargetBPS = m.Bitrate.MediaBytes * 8 * 1_000_000_000 / span
		m.Bitrate.ActualBPS = m.Bitrate.ContainerBytes * 8 * 1_000_000_000 / span
		if m.Bitrate.TargetBPS > 0 {
			dev := float64(m.Bitrate.ActualBPS-m.Bitrate.TargetBPS) / float64(m.Bitrate.TargetBPS) * 100
			if dev < 0 {
				dev = -dev
			}
			m.Bitrate.DeviationPct = dev
		}
	}
	// Quality is purely hash accounting.
	matched := 0
	for _, s := range remuxRes.SampleHashes {
		if s.Hash != ([32]byte{}) {
			matched++
		}
	}
	m.Quality = Quality{Matched: matched, Total: len(remuxRes.SampleHashes)}
	if m.Quality.Total > 0 {
		m.Quality.MatchRatePct = 100 * float64(matched) / float64(m.Quality.Total)
	}
	return m
}

func cropString(c model.Crop) string {
	if !c.Valid {
		return "none"
	}
	return fmt.Sprintf("%dx%d+%d+%d", c.W, c.H, c.X, c.Y)
}

func colorString(c model.ColorDesc) string {
	return fmt.Sprintf("%s/%s/%s/%s", c.Primaries, c.Transfer, c.Matrix, c.FullRange)
}

// Write serializes the manifest to path.
func Write(path string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Read parses a manifest from disk.
func Read(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}
