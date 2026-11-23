// Package manifest parses the asset manifest (JSON) that describes each
// track's raw timing metadata: timescale, samples and edit list. The parsed
// metadata is handed to the timeline package for repair and timebase
// unification. All validation failures return *media.Error carrying the
// track id and the input offset (sample index or JSON byte offset).
package manifest

import (
	"bytes"
	"encoding/json"
	"io"

	"archive/internal/media"
)

// RawSample is one sample as declared by the manifest, in track timescale
// ticks. Values are unverified until timeline repair.
type RawSample struct {
	DTS       int64 `json:"dts"`
	CTSOffset int64 `json:"cts_offset"`
	Duration  int64 `json:"duration"`
	Size      uint32 `json:"size"`
	Keyframe  bool  `json:"keyframe"`
}

// RawEdit is one edit-list entry. MediaTime -1 means an empty (dwell) edit;
// SegmentDuration is expressed in MovieTimescale units.
type RawEdit struct {
	MediaTime       int64 `json:"media_time"`
	SegmentDuration int64 `json:"segment_duration"`
}

// RawTrack is the manifest view of one essence track.
type RawTrack struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"` // "video" | "audio"
	Timescale uint32      `json:"timescale"`
	Samples   []RawSample `json:"samples"`
	EditList  []RawEdit   `json:"edit_list"`
}

// Manifest is the root document. ProgramTracks lists the tracks the program
// timeline expects; a missing entry is a "missing track" error.
type Manifest struct {
	Asset          string     `json:"asset"`
	MovieTimescale uint32     `json:"movie_timescale"`
	ProgramTracks  []string   `json:"program_tracks"`
	Tracks         []RawTrack `json:"tracks"`
}

// Parse reads and validates a manifest. The returned Manifest is
// structurally valid; per-sample timing sanity is left to timeline.Repair.
func Parse(r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, media.New(media.KindInternal, "manifest.parse", "", -1, "read manifest", err)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		off := int64(-1)
		if se, ok := err.(*json.SyntaxError); ok {
			off = se.Offset
		} else if err == io.ErrUnexpectedEOF {
			off = int64(len(data))
		}
		return nil, media.New(media.KindCorrupt, "manifest.parse", "", off, "invalid manifest JSON", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.MovieTimescale == 0 {
		return media.Corruptf("manifest.parse", "", 0, "movie_timescale must be > 0")
	}
	byID := map[string]bool{}
	for _, tr := range m.Tracks {
		byID[tr.ID] = true
	}
	for i, id := range m.ProgramTracks {
		if !byID[id] {
			return media.Corruptf("manifest.parse", id, int64(i), "program references missing track")
		}
	}
	for _, tr := range m.Tracks {
		if tr.Timescale == 0 {
			return media.Corruptf("manifest.parse", tr.ID, -1, "timescale must be > 0")
		}
		if tr.Type != "video" && tr.Type != "audio" {
			return media.Corruptf("manifest.parse", tr.ID, -1, "unsupported track type "+tr.Type)
		}
		for j, s := range tr.Samples {
			if s.Duration < 0 {
				return media.Corruptf("manifest.parse", tr.ID, int64(j), "negative sample duration")
			}
			if s.DTS < 0 {
				return media.Corruptf("manifest.parse", tr.ID, int64(j), "negative sample DTS")
			}
		}
		for j, e := range tr.EditList {
			if e.SegmentDuration < 0 {
				return media.Corruptf("manifest.parse", tr.ID, int64(j), "negative edit segment duration")
			}
			if e.MediaTime < -1 {
				return media.Corruptf("manifest.parse", tr.ID, int64(j), "edit media_time < -1")
			}
		}
	}
	return nil
}

// Track returns the track with the given id.
func (m *Manifest) Track(id string) *RawTrack {
	for i := range m.Tracks {
		if m.Tracks[i].ID == id {
			return &m.Tracks[i]
		}
	}
	return nil
}
