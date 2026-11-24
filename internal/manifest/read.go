// Package manifest reads the media manifest that ties together multiple MP4
// sources, their edit lists and their non-zero presentation origins. Parsing
// keeps byte offsets so any validation failure can be reported with the track
// id and the exact position inside the manifest file.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	merr "mediaroute/internal/errors"
)

// Limits guarding against hostile or accidental oversized manifests.
const (
	// MaxManifestBytes caps the manifest size accepted by Read.
	MaxManifestBytes = 4 << 20
	// MaxTrackCount caps the number of tracks in one manifest.
	MaxTrackCount = 32
	// MaxEditEntries caps edits per track.
	MaxEditEntries = 64
)

// Track roles supported by the pipeline.
const (
	KindVideo = "video"
	KindAudio = "audio"
)

// EditEntry is a manifest-declared edit list entry. Both fields are in
// milliseconds and must be non-negative; MediaTime selects the first sample.
type EditEntry struct {
	SegmentDurationMS int64 `json:"segment_duration_ms"`
	MediaTimeMS       int64 `json:"media_time_ms"`
}

// AudioSpec carries the expected source audio layout so the converter can
// make an explicit, auditable fallback decision when the layout is unknown.
type AudioSpec struct {
	SampleRate   int `json:"sample_rate"`
	ChannelCount int `json:"channel_count"`
	// Layout is "mono", "stereo", "5.1" or "unknown". An empty value means the
	// layout was not declared and triggers the documented fallback policy.
	Layout string `json:"layout"`
}

// Track describes one input track.
type Track struct {
	ID       uint32      `json:"id"`
	Kind     string      `json:"kind"`
	Source   string      `json:"source"`
	Required bool        `json:"required"`
	OriginMS int64       `json:"origin_ms"`
	Edits    []EditEntry `json:"edits,omitempty"`
	Audio    *AudioSpec  `json:"audio,omitempty"`

	// Offset is the byte offset of this track object inside the manifest.
	Offset int64 `json:"-"`
}

// Manifest is a validated media manifest plus its digest and track offsets.
type Manifest struct {
	Version int     `json:"version"`
	Tracks  []Track `json:"tracks"`

	// Digest is the lowercase hex SHA-256 of the raw manifest bytes.
	Digest string `json:"-"`
	// Offsets maps track id to the byte offset of its JSON object.
	Offsets map[uint32]int64 `json:"-"`
}

// ReadFile loads and validates a manifest from disk.
func ReadFile(path string) (*Manifest, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, merr.New(merr.KindManifest, 0, -1, -1, false, "stat %s: %v", path, err)
	}
	if st.Size() > MaxManifestBytes {
		return nil, merr.New(merr.KindManifest, 0, -1, -1, false, "manifest %s exceeds %d bytes", path, MaxManifestBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, merr.New(merr.KindManifest, 0, -1, -1, false, "open %s: %v", path, err)
	}
	defer f.Close()
	return Read(f)
}

// Read parses and validates a manifest, reporting byte offsets on failure.
func Read(r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return nil, merr.New(merr.KindManifest, 0, -1, -1, false, "read manifest: %v", err)
	}
	if len(data) > MaxManifestBytes {
		return nil, merr.New(merr.KindManifest, 0, -1, -1, false, "manifest exceeds %d bytes", MaxManifestBytes)
	}
	m := &Manifest{Offsets: map[uint32]int64{}}
	if err := m.parse(data); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	m.Digest = hex.EncodeToString(sum[:])
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manifest) parse(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return merr.New(merr.KindManifest, 0, -1, 0, false, "decode manifest: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return merr.New(merr.KindManifest, 0, -1, 0, false, "manifest root must be a JSON object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return merr.New(merr.KindManifest, 0, -1, dec.InputOffset(), false, "decode key: %v", err)
		}
		key, _ := keyTok.(string)
		switch key {
		case "version":
			if err := dec.Decode(&m.Version); err != nil {
				return offsetErr(dec.InputOffset(), "version", err)
			}
		case "tracks":
			if err := m.parseTracks(dec, data); err != nil {
				return err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return offsetErr(dec.InputOffset(), key, err)
			}
		}
	}
	if len(m.Tracks) == 0 {
		return merr.New(merr.KindManifest, 0, -1, 0, false, "manifest has no tracks")
	}
	return nil
}

func (m *Manifest) parseTracks(dec *json.Decoder, data []byte) error {
	tok, err := dec.Token()
	if err != nil {
		return offsetErr(dec.InputOffset(), "tracks", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return merr.New(merr.KindManifest, 0, -1, dec.InputOffset(), false, `"tracks" must be an array`)
	}
	for dec.More() {
		if len(m.Tracks) >= MaxTrackCount {
			return merr.New(merr.KindManifest, 0, -1, dec.InputOffset(), false, "track count exceeds limit %d", MaxTrackCount)
		}
		start := trimOffset(data, dec.InputOffset())
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return merr.New(merr.KindManifest, 0, -1, start, false, "decode track: %v", err)
		}
		var t Track
		if err := json.Unmarshal(raw, &t); err != nil {
			return merr.New(merr.KindManifest, 0, -1, start, false, "decode track: %v", err)
		}
		t.Offset = start
		m.Tracks = append(m.Tracks, t)
	}
	if _, err := dec.Token(); err != nil {
		return offsetErr(dec.InputOffset(), "tracks", err)
	}
	return nil
}

func (m *Manifest) validate() error {
	if m.Version != 1 {
		return merr.New(merr.KindManifest, 0, -1, 0, false, "unsupported manifest version %d", m.Version)
	}
	seen := map[uint32]bool{}
	mediaSeen := map[string]uint32{}
	for i := range m.Tracks {
		t := &m.Tracks[i]
		if t.ID == 0 {
			return merr.New(merr.KindManifest, 0, -1, t.Offset, false, "track id must be non-zero")
		}
		if seen[t.ID] {
			return merr.New(merr.KindManifest, t.ID, -1, t.Offset, false, "duplicate track id %d", t.ID)
		}
		seen[t.ID] = true
		m.Offsets[t.ID] = t.Offset
		if t.Kind != KindVideo && t.Kind != KindAudio {
			return merr.New(merr.KindManifest, t.ID, -1, t.Offset, false, "unsupported track kind %q", t.Kind)
		}
		if t.Source == "" {
			return merr.New(merr.KindManifest, t.ID, -1, t.Offset, false, "track %d has no source file", t.ID)
		}
		if prev, ok := mediaSeen[t.Source]; ok {
			return merr.New(merr.KindManifest, t.ID, -1, t.Offset, false, "source %q already referenced by track %d", t.Source, prev)
		}
		mediaSeen[t.Source] = t.ID
		if t.OriginMS < 0 {
			return merr.New(merr.KindManifest, t.ID, -1, t.Offset, false, "negative origin %d ms", t.OriginMS)
		}
		if len(t.Edits) > MaxEditEntries {
			return merr.New(merr.KindManifest, t.ID, -1, t.Offset, false, "edit list exceeds %d entries", MaxEditEntries)
		}
		var total int64
		for j, e := range t.Edits {
			if e.SegmentDurationMS < 0 {
				return merr.New(merr.KindManifest, t.ID, int64(j), t.Offset, false, "negative edit duration %d ms", e.SegmentDurationMS)
			}
			if e.MediaTimeMS < 0 {
				return merr.New(merr.KindManifest, t.ID, int64(j), t.Offset, false, "negative edit media time %d ms", e.MediaTimeMS)
			}
			if total > (1<<62)-e.SegmentDurationMS {
				return merr.New(merr.KindManifest, t.ID, int64(j), t.Offset, false, "edit duration sum overflows int64")
			}
			total += e.SegmentDurationMS
		}
		if t.Audio != nil {
			if t.Kind != KindAudio {
				return merr.New(merr.KindManifest, t.ID, -1, t.Offset, false, "audio spec on non-audio track")
			}
			if t.Audio.SampleRate <= 0 || t.Audio.SampleRate > 384000 {
				return merr.New(merr.KindManifest, t.ID, -1, t.Offset, false, "invalid source sample rate %d", t.Audio.SampleRate)
			}
			if t.Audio.ChannelCount <= 0 || t.Audio.ChannelCount > 32 {
				return merr.New(merr.KindManifest, t.ID, -1, t.Offset, false, "invalid source channel count %d", t.Audio.ChannelCount)
			}
		}
	}
	required := m.RequiredIDs()
	if len(required) == 0 {
		return merr.New(merr.KindManifest, 0, -1, 0, false, "manifest declares no required tracks")
	}
	return nil
}

// TrackByID returns the track with the given id.
func (m *Manifest) TrackByID(id uint32) (Track, bool) {
	for _, t := range m.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return Track{}, false
}

// RequiredIDs returns the ids of all tracks flagged as required.
func (m *Manifest) RequiredIDs() []uint32 {
	ids := make([]uint32, 0, len(m.Tracks))
	for _, t := range m.Tracks {
		if t.Required {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// TrackOffset returns the recorded byte offset for a track id, or -1.
func (m *Manifest) TrackOffset(id uint32) int64 {
	if off, ok := m.Offsets[id]; ok {
		return off
	}
	return -1
}

func trimOffset(data []byte, off int64) int64 {
	if off < 0 {
		return 0
	}
	for off < int64(len(data)) {
		switch data[off] {
		case ' ', '\t', '\r', '\n':
			off++
		default:
			return off
		}
	}
	return off
}

func offsetErr(off int64, field string, err error) *merr.Error {
	return merr.New(merr.KindManifest, 0, -1, off, false, "decode %q: %v", field, err)
}

// String renders a compact summary used in terminal output.
func (m *Manifest) String() string {
	return fmt.Sprintf("manifest v%d tracks=%d digest=%s", m.Version, len(m.Tracks), m.Digest[:12])
}
