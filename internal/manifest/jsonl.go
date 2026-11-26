// Package manifest parses the machine-readable media manifest (JSONL) and the
// editorial cut list, and validates every field the rest of the pipeline
// trusts: time bases, wrap widths, anchors, paths and edit ranges.
package manifest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediad/internal/media"
)

// Anchor pins a media timestamp to a shared host wall clock so drift can be
// fitted per camera. HostUS is host microseconds, MediaTicks are raw ticks in
// the entry's time base.
type Anchor struct {
	HostUS     int64 `json:"host_us"`
	MediaTicks int64 `json:"media_ticks"`
}

// Entry is one line of the media manifest.
type Entry struct {
	Camera      string      `json:"camera"`
	File        string      `json:"file"`
	Track       string      `json:"track"`
	Timebase    string      `json:"timebase"`
	WrapBits    int         `json:"wrap_bits"`
	Anchors     []Anchor    `json:"anchors"`
	Layout      string      `json:"layout,omitempty"`
	MixMatrix   [][]float64 `json:"mix_matrix,omitempty"`
	FrameRate   string      `json:"frame_rate,omitempty"`
	StreamIndex *int        `json:"stream_index,omitempty"`
	SampleRate  int         `json:"sample_rate,omitempty"`
	Channels    int         `json:"channels,omitempty"`

	tb media.Rational
}

// TimeBase returns the parsed rational time base of the entry.
func (e *Entry) TimeBase() media.Rational { return e.tb }

// Key is the stable identity of a camera track.
func (e *Entry) Key() string { return e.Camera + "/" + e.Track }

// Manifest is the ordered collection of entries plus an index.
type Manifest struct {
	Entries []*Entry
	byKey   map[string]*Entry
}

// Lookup returns the entry for camera/track.
func (m *Manifest) Lookup(camera, track string) (*Entry, bool) {
	e, ok := m.byKey[camera+"/"+track]
	return e, ok
}

// ParseManifest reads a JSONL manifest. Blank lines and lines beginning with
// '#' are ignored. Every line is validated; the first failure aborts parsing.
func ParseManifest(r io.Reader) (*Manifest, error) {
	m := &Manifest{byKey: map[string]*Entry{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			return nil, media.NewError(media.ErrManifest, "line %d: %v", line, err)
		}
		if err := e.validate(); err != nil {
			me, _ := media.AsMediaError(err)
			if me != nil {
				me.Message = fmt.Sprintf("line %d: %s", line, me.Message)
			}
			return nil, err
		}
		if _, dup := m.byKey[e.Key()]; dup {
			return nil, media.NewError(media.ErrManifest, "line %d: duplicate track %s", line, e.Key())
		}
		m.byKey[e.Key()] = &e
		m.Entries = append(m.Entries, &e)
	}
	if err := sc.Err(); err != nil {
		return nil, media.NewError(media.ErrManifest, "reading manifest: %v", err)
	}
	sort.SliceStable(m.Entries, func(i, j int) bool {
		if m.Entries[i].Camera != m.Entries[j].Camera {
			return m.Entries[i].Camera < m.Entries[j].Camera
		}
		return m.Entries[i].Track < m.Entries[j].Track
	})
	return m, nil
}

// ParseManifestFile opens and parses a manifest from disk.
func ParseManifestFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, media.NewError(media.ErrIO, "opening manifest %s: %v", path, err)
	}
	defer f.Close()
	return ParseManifest(f)
}

func (e *Entry) validate() error {
	if strings.TrimSpace(e.Camera) == "" {
		return media.NewError(media.ErrManifest, "camera is empty")
	}
	if strings.TrimSpace(e.Track) == "" {
		return media.NewError(media.ErrManifest, "track is empty")
	}
	if strings.TrimSpace(e.File) == "" {
		return media.NewError(media.ErrManifest, "file is empty")
	}
	tb, err := media.ParseRational(e.Timebase)
	if err != nil {
		return err
	}
	e.tb = tb
	if e.WrapBits == 0 {
		return media.NewError(media.ErrManifest, "track %s: wrap_bits missing", e.Key())
	}
	if e.WrapBits < 16 || e.WrapBits > 62 {
		return media.NewError(media.ErrManifest, "track %s: wrap_bits %d outside [16,62]", e.Key(), e.WrapBits)
	}
	if len(e.Anchors) == 0 {
		return media.NewError(media.ErrManifest, "track %s: no sync anchors", e.Key())
	}
	for i, a := range e.Anchors {
		if a.HostUS < 0 {
			return media.NewError(media.ErrManifest, "track %s: anchor %d has negative host time", e.Key(), i)
		}
		if i > 0 && a.HostUS <= e.Anchors[i-1].HostUS {
			return media.NewError(media.ErrManifest, "track %s: anchors not increasing at %d", e.Key(), i)
		}
	}
	if e.SampleRate < 0 || e.SampleRate > 384000 {
		return media.NewError(media.ErrManifest, "track %s: sample_rate %d invalid", e.Key(), e.SampleRate)
	}
	if e.Channels < 0 || e.Channels > 64 {
		return media.NewError(media.ErrManifest, "track %s: channels %d invalid", e.Key(), e.Channels)
	}
	if len(e.MixMatrix) > 0 && e.Layout == "" {
		return media.NewError(media.ErrManifest, "track %s: mix_matrix without layout", e.Key())
	}
	for i, row := range e.MixMatrix {
		if len(row) == 0 {
			return media.NewError(media.ErrManifest, "track %s: mix_matrix row %d empty", e.Key(), i)
		}
		for _, v := range row {
			if v < -8 || v > 8 {
				return media.NewError(media.ErrManifest, "track %s: mix_matrix coefficient %.3f out of range", e.Key(), v)
			}
		}
	}
	if e.FrameRate != "" {
		if _, err := media.ParseRational(e.FrameRate); err != nil {
			return media.NewError(media.ErrInvalidTimebase, "track %s: frame_rate %q: %v", e.Key(), e.FrameRate, err)
		}
	}
	return nil
}

// ResolveWithin resolves rel against root and guarantees the result stays
// inside root even when rel or an intermediate component is a symlink.
func ResolveWithin(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", media.NewError(media.ErrPathEscape, "absolute path %q not allowed", rel)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", media.NewError(media.ErrPathEscape, "path %q escapes media root", rel)
	}
	ext := strings.ToLower(filepath.Ext(clean))
	if ext != ".mov" && ext != ".mp4" && ext != ".m4a" {
		return "", media.NewError(media.ErrManifest, "path %q is not a MOV/MP4 asset", rel)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", media.NewError(media.ErrIO, "resolving root: %v", err)
	}
	absRoot, err = filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", media.NewError(media.ErrIO, "resolving root symlinks: %v", err)
	}
	candidate := filepath.Join(absRoot, clean)
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", media.NewError(media.ErrIO, "resolving %q: %v", rel, err)
	}
	if resolved != absRoot && !strings.HasPrefix(resolved, absRoot+string(filepath.Separator)) {
		return "", media.NewError(media.ErrPathEscape, "path %q resolves outside media root", rel)
	}
	return resolved, nil
}
