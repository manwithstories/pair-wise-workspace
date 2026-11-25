// Package manifest parses JSONL asset manifests. Each line describes one
// track of a source asset, including its edit list. Parse errors always
// carry the source line number and, when known, the track ID.
package manifest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Edit is one edit-list entry: present [MediaStart, MediaStart+Duration)
// of the media timeline, in track timescale ticks.
type Edit struct {
	MediaStart int64 `json:"media_start"`
	Duration   int64 `json:"duration"`
}

// Track is one manifest line.
type Track struct {
	ID        string   `json:"track_id"`
	Path      string   `json:"path"`
	Kind      string   `json:"kind"` // "video" or "audio"
	Timescale int64    `json:"timescale"`
	DependsOn []string `json:"depends_on"`
	Edits     []Edit   `json:"edits"`

	Line int `json:"-"` // 1-based source line
}

// Manifest is an ordered set of tracks.
type Manifest struct {
	Tracks []Track
}

// ParseError reports a bad manifest line.
type ParseError struct {
	Line  int
	Track string // may be empty if the line had no usable track_id
	Msg   string
}

func (e *ParseError) Error() string {
	if e.Track != "" {
		return fmt.Sprintf("manifest line %d (track %q): %s", e.Line, e.Track, e.Msg)
	}
	return fmt.Sprintf("manifest line %d: %s", e.Line, e.Msg)
}

// Find returns the track with the given ID.
func (m *Manifest) Find(id string) *Track {
	for i := range m.Tracks {
		if m.Tracks[i].ID == id {
			return &m.Tracks[i]
		}
	}
	return nil
}

// Parse reads a JSONL manifest. The first invalid line aborts parsing.
func Parse(r io.Reader) (*Manifest, error) {
	m := &Manifest{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var tr Track
		if err := json.Unmarshal([]byte(text), &tr); err != nil {
			return nil, &ParseError{Line: line, Msg: "invalid JSON: " + err.Error()}
		}
		tr.Line = line
		if err := validate(&tr, seen); err != nil {
			return nil, err
		}
		seen[tr.ID] = true
		m.Tracks = append(m.Tracks, tr)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(m.Tracks) == 0 {
		return nil, &ParseError{Msg: "manifest contains no tracks"}
	}
	return m, nil
}

func validate(tr *Track, seen map[string]bool) *ParseError {
	bad := func(format string, args ...any) *ParseError {
		return &ParseError{Line: tr.Line, Track: tr.ID, Msg: fmt.Sprintf(format, args...)}
	}
	if tr.ID == "" {
		return bad("missing track_id")
	}
	if seen[tr.ID] {
		return bad("duplicate track_id")
	}
	if tr.Kind != "video" && tr.Kind != "audio" {
		return bad("kind must be \"video\" or \"audio\", got %q", tr.Kind)
	}
	if tr.Timescale <= 0 {
		return bad("timescale must be positive, got %d", tr.Timescale)
	}
	if tr.Path == "" {
		return bad("missing path")
	}
	if !filepath.IsLocal(tr.Path) {
		return bad("path %q must be relative and stay inside the work directory", tr.Path)
	}
	for i, e := range tr.Edits {
		if e.MediaStart < 0 {
			return bad("edit %d: negative media_start %d", i, e.MediaStart)
		}
		if e.Duration <= 0 {
			return bad("edit %d: non-positive duration %d", i, e.Duration)
		}
	}
	return nil
}
