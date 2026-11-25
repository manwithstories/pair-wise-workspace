// Package ingest scans pre-encoded H.264/H.265/AV1 bare streams, validates
// the supplied keyframe manifests (JSON or TSV) and produces access units
// aligned to the shared 90 kHz clock.
package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/fieldcut/fieldcut/internal/base"
)

// Codec identifies the bare-stream coding format.
type Codec string

const (
	CodecH264 Codec = "h264"
	CodecH265 Codec = "h265"
	CodecAV1  Codec = "av1"
)

// IndexEntry is one manifest row: where an access unit lives in the file and
// what the encoder claimed its timestamp was.
type IndexEntry struct {
	Camera    string `json:"camera"`
	Rendition string `json:"rendition"`
	Codec     Codec  `json:"codec"`
	TimeBase  uint64 `json:"time_base"`
	PTS       uint64 `json:"pts"`
	Offset    int64  `json:"offset"`
	Length    int    `json:"length"`
	Keyframe  bool   `json:"keyframe"`
}

// jsonManifest is the on-disk JSON envelope.
type jsonManifest struct {
	Camera    string      `json:"camera"`
	Rendition string      `json:"rendition"`
	Codec     Codec       `json:"codec"`
	TimeBase  uint64      `json:"time_base"`
	Entries   []jsonIndex `json:"entries"`
}

type jsonIndex struct {
	PTS      uint64 `json:"pts"`
	Offset   int64  `json:"offset"`
	Length   int    `json:"length"`
	Keyframe bool   `json:"keyframe"`
}

// Manifest is the validated in-memory index for one stream.
type Manifest struct {
	Camera    string
	Rendition string
	Codec     Codec
	TimeBase  uint64
	Entries   []IndexEntry
}

// AccessUnit is one scanned, clock-normalized frame worth of data.
type AccessUnit struct {
	Camera    string
	Rendition string
	Codec     Codec
	// PTS90k is the presentation timestamp on the shared clock.
	PTS90k uint64
	// ClockGap marks an unresolvable clock discontinuity; splicing across
	// such a unit is forbidden.
	ClockGap bool
	Offset   int64
	Payload  []byte
	Keyframe bool
	// Truncated means the unit ran into EOF or a structural error; it is
	// isolated and the scanner resumes at the next valid keyframe.
	Truncated bool
	// Rescanned is set when the unit was recovered by a limited forward
	// rescan rather than trusted from the manifest.
	Rescanned bool
}

// Locator builds a base.Locator for fault reporting.
func (m *Manifest) Locator(e *IndexEntry, pts uint64) base.Locator {
	return base.Locator{Camera: m.Camera, Rendition: m.Rendition, PTS: pts, Offset: e.Offset}
}

// ParseJSONManifest decodes a JSON manifest and validates it against the
// actual stream size.
func ParseJSONManifest(r io.Reader, streamSize int64) (*Manifest, error) {
	var jm jsonManifest
	if err := json.NewDecoder(r).Decode(&jm); err != nil {
		return nil, fmt.Errorf("decode json manifest: %w", err)
	}
	m := &Manifest{
		Camera: jm.Camera, Rendition: jm.Rendition, Codec: jm.Codec,
		TimeBase: jm.TimeBase, Entries: make([]IndexEntry, 0, len(jm.Entries)),
	}
	for _, e := range jm.Entries {
		m.Entries = append(m.Entries, IndexEntry{
			Camera: jm.Camera, Rendition: jm.Rendition, Codec: jm.Codec,
			TimeBase: jm.TimeBase, PTS: e.PTS, Offset: e.Offset,
			Length: e.Length, Keyframe: e.Keyframe,
		})
	}
	return m, m.Validate(streamSize)
}

// ParseTSVManifest decodes TSV rows: camera rendition codec time_base pts
// offset length keyframe, one unit per line. '#' starts a comment.
func ParseTSVManifest(r io.Reader, streamSize int64) (*Manifest, error) {
	m := &Manifest{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 8 {
			return nil, fmt.Errorf("tsv manifest: expected 8 columns, got %d: %q", len(f), line)
		}
		tb, err := strconv.ParseUint(f[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("tsv manifest: bad time_base: %w", err)
		}
		pts, err := strconv.ParseUint(f[4], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("tsv manifest: bad pts: %w", err)
		}
		off, err := strconv.ParseInt(f[5], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("tsv manifest: bad offset: %w", err)
		}
		n, err := strconv.Atoi(f[6])
		if err != nil {
			return nil, fmt.Errorf("tsv manifest: bad length: %w", err)
		}
		key := f[7] == "1" || strings.EqualFold(f[7], "true")
		if m.Camera == "" {
			m.Camera, m.Rendition, m.Codec, m.TimeBase = f[0], f[1], Codec(f[2]), tb
		}
		m.Entries = append(m.Entries, IndexEntry{
			Camera: f[0], Rendition: f[1], Codec: Codec(f[2]), TimeBase: tb,
			PTS: pts, Offset: off, Length: n, Keyframe: key,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read tsv manifest: %w", err)
	}
	return m, m.Validate(streamSize)
}

// Validate enforces size limits, in-file ranges and unique, ordered offsets.
func (m *Manifest) Validate(streamSize int64) error {
	if m.TimeBase == 0 {
		return base.NewFault(base.FaultIndexDrift, base.Locator{
			Camera: m.Camera, Rendition: m.Rendition}, false, "zero time base")
	}
	seen := make(map[int64]struct{}, len(m.Entries))
	var prevOff int64
	for i := range m.Entries {
		e := &m.Entries[i]
		loc := base.Locator{Camera: m.Camera, Rendition: m.Rendition, PTS: e.PTS, Offset: e.Offset}
		if e.Length <= 0 {
			return base.NewFault(base.FaultIndexDrift, loc, false, "entry %d: non-positive length %d", i, e.Length)
		}
		if e.Length > base.MaxAccessUnitBytes {
			return base.NewFault(base.FaultOversize, loc, false,
				"entry %d: access unit %d bytes exceeds 16 MiB", i, e.Length)
		}
		if e.Offset < 0 || e.Offset+int64(e.Length) > streamSize {
			return base.NewFault(base.FaultIndexDrift, loc, false,
				"entry %d: [%d,+%d) outside stream of %d bytes", i, e.Offset, e.Length, streamSize)
		}
		if _, dup := seen[e.Offset]; dup {
			return base.NewFault(base.FaultDuplicateIndex, loc, false,
				"entry %d: duplicate offset %d", i, e.Offset)
		}
		seen[e.Offset] = struct{}{}
		if i > 0 && e.Offset <= prevOff {
			return base.NewFault(base.FaultIndexDrift, loc, false,
				"entry %d: offsets not strictly increasing (%d <= %d)", i, e.Offset, prevOff)
		}
		prevOff = e.Offset
	}
	return nil
}

// PTS90k converts an entry's native time-base PTS to the shared clock.
func (m *Manifest) PTS90k(e *IndexEntry) uint64 {
	return e.PTS * base.ClockRate / m.TimeBase
}

// MarshalJSON serializes the manifest for fixture generation and checkpoints.
func (m *Manifest) MarshalJSON() ([]byte, error) {
	jm := jsonManifest{
		Camera: m.Camera, Rendition: m.Rendition, Codec: m.Codec, TimeBase: m.TimeBase,
		Entries: make([]jsonIndex, len(m.Entries)),
	}
	for i, e := range m.Entries {
		jm.Entries[i] = jsonIndex{PTS: e.PTS, Offset: e.Offset, Length: e.Length, Keyframe: e.Keyframe}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&jm); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
