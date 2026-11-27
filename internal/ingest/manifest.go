// Package ingest validates frame manifests, their hashes and paths before
// any frame is decoded. Every rejection uses media.MediaError.
package ingest

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fieldcut/internal/media"
)

// Entry is one manifest row: a frame on a track at a device tick and an
// claimed wall-clock time, with its content hash.
type Entry struct {
	Track  string `json:"track"`
	Seq    int64  `json:"seq"`
	Tick   int64  `json:"tick"`
	Nanos  int64  `json:"nanos"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Track describes a track and its declared bounds.
type Track struct {
	Name   string `json:"name"`
	Offset int64  `json:"offset_tick"`
	Frames int    `json:"frames"`
}

// Manifest is the chunked frame inventory.
type Manifest struct {
	Version   int     `json:"version"`
	OutputFPS int     `json:"output_fps"`
	WrapAt    int64   `json:"wrap_at"`
	Tracks    []Track `json:"tracks"`
	Entries   []Entry `json:"entries"`
	Chunk     int     `json:"chunk"`
}

// Load reads a manifest file, validates every row and returns the
// manifest together with the deduplicated, ordered per-track entries.
func Load(path string) (*Manifest, map[string][]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, media.New(media.KindInput, "manifest.open", "", -1, err)
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	m := &Manifest{}
	if err := dec.Decode(m); err != nil {
		return nil, nil, media.New(media.KindInput, "manifest.decode", "", -1, err)
	}
	sets, err := Validate(m)
	if err != nil {
		return nil, nil, err
	}
	return m, sets, nil
}

// Validate enforces size, path, hash and ordering rules and groups entries
// by track with duplicate sequence numbers removed (first wins).
func Validate(m *Manifest) (map[string][]Entry, error) {
	if m == nil {
		return nil, media.New(media.KindInput, "manifest.validate", "", -1, errors.New("nil manifest"))
	}
	if m.OutputFPS != media.FrameRate {
		return nil, media.Newf(media.KindInput, "manifest.validate", "", -1,
			"output_fps=%d, want %d", m.OutputFPS, media.FrameRate)
	}
	if len(m.Tracks) == 0 || len(m.Tracks) > media.MaxTracks {
		return nil, media.Newf(media.KindLimit, "manifest.tracks", "", -1,
			"track count %d outside [1,%d]", len(m.Tracks), media.MaxTracks)
	}
	if m.WrapAt <= 0 || m.WrapAt > 1<<32 {
		return nil, media.Newf(media.KindClock, "manifest.wrap", "", -1, "wrap_at=%d outside (0,2^32]", m.WrapAt)
	}
	known := make(map[string]bool, len(m.Tracks))
	for _, t := range m.Tracks {
		if t.Name == "" || known[t.Name] {
			return nil, media.Newf(media.KindInput, "manifest.tracks", t.Name, -1, "empty or duplicate track name")
		}
		known[t.Name] = true
	}
	if len(m.Entries) == 0 {
		return nil, media.New(media.KindInput, "manifest.entries", "", -1, errors.New("no entries"))
	}
	out := make(map[string][]Entry, len(m.Tracks))
	seen := make(map[string]map[int64]bool, len(m.Tracks))
	for _, e := range m.Entries {
		if !known[e.Track] {
			return nil, media.Newf(media.KindInput, "manifest.entry", e.Track, e.Seq, "unknown track")
		}
		if e.Seq < 0 {
			return nil, media.Newf(media.KindInput, "manifest.entry", e.Track, e.Seq, "negative sequence")
		}
		if e.Width <= 0 || e.Height <= 0 || e.Width > media.MaxWidth || e.Height > media.MaxHeight {
			return nil, media.Newf(media.KindInput, "manifest.size", e.Track, e.Seq,
				"size %dx%d outside [1,%d]x[1,%d]", e.Width, e.Height, media.MaxWidth, media.MaxHeight)
		}
		if err := SafePath(e.Path); err != nil {
			return nil, media.New(media.KindInput, "manifest.path", e.Track, e.Seq, err)
		}
		if len(e.SHA256) != 64 {
			return nil, media.Newf(media.KindInput, "manifest.hash", e.Track, e.Seq, "sha256 length %d", len(e.SHA256))
		}
		if _, err := hex.DecodeString(e.SHA256); err != nil {
			return nil, media.Newf(media.KindInput, "manifest.hash", e.Track, e.Seq, "sha256 is not hex")
		}
		if seen[e.Track] == nil {
			seen[e.Track] = map[int64]bool{}
		}
		if seen[e.Track][e.Seq] {
			continue // duplicate delivery: first arrival wins
		}
		seen[e.Track][e.Seq] = true
		out[e.Track] = append(out[e.Track], e)
	}
	for name := range out {
		sort.Slice(out[name], func(a, b int) bool { return out[name][a].Seq < out[name][b].Seq })
	}
	for _, t := range m.Tracks {
		if len(out[t.Name]) == 0 {
			return nil, media.New(media.KindInput, "manifest.track", t.Name, -1, errors.New("track has no entries"))
		}
		if t.Frames > 0 && len(out[t.Name]) > t.Frames {
			return nil, media.Newf(media.KindLimit, "manifest.track", t.Name, -1,
				"%d entries exceed declared %d", len(out[t.Name]), t.Frames)
		}
	}
	return out, nil
}

// SafePath rejects absolute paths, parent traversal and drive letters so a
// manifest cannot reach outside its own directory tree.
func SafePath(p string) error {
	if p == "" {
		return errors.New("empty path")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("absolute path %q", p)
	}
	if strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("illegal characters in %q", p)
	}
	if strings.Contains(p, ":") {
		return fmt.Errorf("path %q contains a drive or scheme", p)
	}
	raw := strings.Split(p, string(filepath.Separator))
	for _, part := range raw {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("path %q has an empty or parent segment", p)
		}
	}
	clean := filepath.Clean(p)
	if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes its root", p)
	}
	return nil
}

// HashFile returns the SHA-256 of a file as lowercase hex.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyEntry re-hashes a frame and fails when it does not match the
// manifest, using the corrupt-frame error model.
func VerifyEntry(root string, e Entry) error {
	if err := SafePath(e.Path); err != nil {
		return media.New(media.KindInput, "ingest.path", e.Track, e.Seq, err)
	}
	sum, err := HashFile(filepath.Join(root, e.Path))
	if err != nil {
		return media.New(media.KindInput, "ingest.hash", e.Track, e.Seq, err)
	}
	if sum != strings.ToLower(e.SHA256) {
		return media.Newf(media.KindCorrupt, "ingest.hash", e.Track, e.Seq, "sha256 mismatch")
	}
	return nil
}

// Verdict records a per-entry check: nil Err means the frame decoded and
// matched its declared dimensions. Hash mismatches and missing files are
// fatal; a decodable failure is carried as a corrupt-frame verdict so the
// recovery stage can decide instead of aborting the whole pass.
type Verdict struct {
	Entry Entry
	Err   *media.MediaError
}

// CheckFrames verifies hashes and decodes every entry. Fatal input errors
// (missing file, hash mismatch) are returned immediately. Decode failures
// and dimension mismatches are reported per entry with KindCorrupt.
func CheckFrames(root string, entries map[string][]Entry) (map[string]map[int64]*media.MediaError, error) {
	out := make(map[string]map[int64]*media.MediaError, len(entries))
	for track, list := range entries {
		out[track] = make(map[int64]*media.MediaError, len(list))
		for _, e := range list {
			if err := VerifyEntry(root, e); err != nil {
				return nil, err
			}
			w, h, err := decodeSize(filepath.Join(root, e.Path))
			if err != nil {
				out[track][e.Seq] = media.New(media.KindCorrupt, "ingest.decode", track, e.Seq, err)
				continue
			}
			if w != e.Width || h != e.Height {
				out[track][e.Seq] = media.Newf(media.KindCorrupt, "ingest.decode", track, e.Seq,
					"decoded %dx%d, manifest says %dx%d", w, h, e.Width, e.Height)
			}
		}
	}
	return out, nil
}

func decodeSize(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}
