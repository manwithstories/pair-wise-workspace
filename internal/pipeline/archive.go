package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"tsqc/internal/clock"
)

// Manifest is the on-disk contract checked by `tsqc verify`.
type Manifest struct {
	Version         int             `json:"version"`
	Source          string          `json:"source"`
	Seed            int64           `json:"seed"`
	Faults          []string        `json:"faults"`
	InputSHA        string          `json:"input_sha256"`
	Roundtrip       float64         `json:"roundtrip_consistency"`
	KeyframeHitRate float64         `json:"keyframe_hit_rate"`
	Recovered       int             `json:"gaps_recovered"`
	Unrecoverable   int             `json:"gaps_unrecoverable"`
	Segments        []ManifestEntry `json:"segments"`
}

// ManifestEntry is one archived segment.
type ManifestEntry struct {
	Index      int    `json:"index"`
	File       string `json:"file"`
	SHA256     string `json:"sha256"`
	ContentSHA string `json:"content_sha256"`
	StartPTS   int64  `json:"start_pts"`
	EndPTS     int64  `json:"end_pts"`
	VideoAUs   int    `json:"video_aus"`
	AudioAUs   int    `json:"audio_aus"`
	GapBack    bool   `json:"gap_back"`
	Size       int    `json:"size"`
}

func writeManifest(dir string, rep *Report, segs []Segment) error {
	m := Manifest{
		Version: 1, Source: rep.Source, Seed: rep.Seed, Faults: rep.Faults,
		InputSHA: rep.InputSHA, Roundtrip: rep.Roundtrip,
		KeyframeHitRate: rep.KeyframeHitRate, Recovered: rep.Recovered,
		Unrecoverable: rep.Unrecoverable,
	}
	// The manifest is self-contained; verify recomputes the file digests.
	for _, s := range segs {
		m.Segments = append(m.Segments, ManifestEntry{
			Index: s.Index, File: fmt.Sprintf("seg-%02d.ts", s.Index),
			SHA256: s.Digest(), ContentSHA: s.ContentSHA,
			StartPTS: s.StartPTS, EndPTS: s.EndPTS,
			VideoAUs: s.VideoAUs, AudioAUs: s.AudioAUs, GapBack: s.GapBack, Size: s.Size,
		})
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644)
}

// ReadManifest loads a manifest written by Run.
func ReadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, clock.FatalError(clock.CodeDigestMismatch, "pipeline.ReadManifest", "%v", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, clock.FatalError(clock.CodeDigestMismatch, "pipeline.ReadManifest", "corrupt manifest: %v", err)
	}
	sort.Slice(m.Segments, func(i, j int) bool { return m.Segments[i].Index < m.Segments[j].Index })
	return &m, nil
}

// VerifyDir checks every archived file against its manifest digest and
// reparses it for track, timestamp and packet-order integrity.
func VerifyDir(dir string) (*Manifest, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	if len(m.Segments) == 0 {
		return nil, clock.FatalError(clock.CodeDigestMismatch, "pipeline.VerifyDir", "manifest has no segments")
	}
	for _, e := range m.Segments {
		data, err := os.ReadFile(filepath.Join(dir, e.File))
		if err != nil {
			return nil, clock.FatalError(clock.CodeDigestMismatch, "pipeline.VerifyDir", "%s: %v", e.File, err)
		}
		if digestOf(data) != e.SHA256 {
			return nil, clock.FatalError(clock.CodeDigestMismatch, "pipeline.VerifyDir", "%s sha256 mismatch", e.File)
		}
		seg := &Segment{
			Index: e.Index, StartPTS: e.StartPTS, EndPTS: e.EndPTS,
			VideoAUs: e.VideoAUs, AudioAUs: e.AudioAUs, Bytes: data,
			SHA256: e.SHA256, ContentSHA: e.ContentSHA, Size: len(data),
		}
		if err := VerifySegment(seg); err != nil {
			return nil, err
		}
	}
	return m, nil
}
