package manifest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"mediad/internal/media"
)

// Edit describes one editorial cut: a half-open range [In, Out) expressed in
// the entry's raw media ticks, tagged with the camera and track it selects.
type Edit struct {
	Camera string `json:"camera"`
	Track  string `json:"track"`
	In     int64  `json:"in"`
	Out    int64  `json:"out"`
	Seq    int64  `json:"seq"`
	Label  string `json:"label,omitempty"`
}

// Duration is Out-In in raw ticks; always positive after validation.
func (e Edit) Duration() int64 { return e.Out - e.In }

// Key matches the manifest entry identity.
func (e Edit) Key() string { return e.Camera + "/" + e.Track }

// EditList is the validated, ordered editorial cut list.
type EditList struct {
	Edits []Edit
}

// ParseEdits reads a JSONL edit list. In/Out may also be given as
// "num/den" strings; they are normalised to integer raw ticks.
func ParseEdits(r io.Reader) (*EditList, error) {
	el := &EditList{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var raw struct {
			Camera string          `json:"camera"`
			Track  string          `json:"track"`
			In     json.RawMessage `json:"in"`
			Out    json.RawMessage `json:"out"`
			Seq    int64           `json:"seq"`
			Label  string          `json:"label,omitempty"`
		}
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&raw); err != nil {
			return nil, media.NewError(media.ErrManifest, "edits line %d: %v", line, err)
		}
		in, err := parseTick(raw.In)
		if err != nil {
			return nil, media.NewError(media.ErrManifest, "edits line %d: in: %v", line, err)
		}
		out, err := parseTick(raw.Out)
		if err != nil {
			return nil, media.NewError(media.ErrManifest, "edits line %d: out: %v", line, err)
		}
		e := Edit{Camera: strings.TrimSpace(raw.Camera), Track: strings.TrimSpace(raw.Track), In: in, Out: out, Seq: raw.Seq, Label: raw.Label}
		if e.Camera == "" || e.Track == "" {
			return nil, media.NewError(media.ErrManifest, "edits line %d: camera/track required", line)
		}
		if e.Out <= e.In {
			return nil, media.NewError(media.ErrManifest, "edits line %d: negative or zero duration (%d -> %d)", line, e.In, e.Out)
		}
		el.Edits = append(el.Edits, e)
	}
	if err := sc.Err(); err != nil {
		return nil, media.NewError(media.ErrManifest, "reading edits: %v", err)
	}
	if len(el.Edits) == 0 {
		return nil, media.NewError(media.ErrManifest, "edit list is empty")
	}
	sort.SliceStable(el.Edits, func(i, j int) bool {
		a, b := el.Edits[i], el.Edits[j]
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		if a.Camera != b.Camera {
			return a.Camera < b.Camera
		}
		return a.Track < b.Track
	})
	return el, nil
}

// ParseEditsFile opens and parses an edit list from disk.
func ParseEditsFile(path string) (*EditList, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, media.NewError(media.ErrIO, "opening edits %s: %v", path, err)
	}
	defer f.Close()
	return ParseEdits(f)
}

func parseTick(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("missing value")
	}
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "\"") {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, err
		}
		if strings.ContainsAny(str, "/:") {
			r, err := media.ParseRational(str)
			if err != nil {
				return 0, err
			}
			// The caller's time base is applied later; keep the ratio exact by
			// storing numerator and denominator as a fraction of a tick.
			if r.Den != 1 {
				return 0, fmt.Errorf("fractional tick %q", str)
			}
			return r.Num, nil
		}
		var v int64
		if _, err := fmt.Sscanf(str, "%d", &v); err != nil {
			return 0, err
		}
		return v, nil
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, err
	}
	return v, nil
}

// ValidateAgainst checks that every edit refers to a known manifest entry and
// that no two edits on the same track overlap.
func (el *EditList) ValidateAgainst(m *Manifest) error {
	byTrack := map[string][]Edit{}
	for _, e := range el.Edits {
		entry, ok := m.Lookup(e.Camera, e.Track)
		if !ok {
			return media.NewError(media.ErrManifest, "edit references unknown track %s", e.Key())
		}
		_ = entry
		byTrack[e.Key()] = append(byTrack[e.Key()], e)
	}
	for key, edits := range byTrack {
		sort.Slice(edits, func(i, j int) bool { return edits[i].In < edits[j].In })
		for i := 1; i < len(edits); i++ {
			if edits[i].In < edits[i-1].Out {
				return media.NewError(media.ErrManifest, "track %s: edits overlap at %d", key, edits[i].In)
			}
		}
	}
	return nil
}
