// Package index persists a demux scan as a checked cache: keyframes, timed
// metadata and chapter positions bound to the input's SHA-256 digest.
//
// An index is valid only for the exact input bytes it was built from. When the
// digest changes the cached entry is invalidated atomically and rebuilt, so a
// stale index can never steer the pipeline onto a shifted timeline.
package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mediacore/internal/clock"
	"mediacore/internal/meta"
	"mediacore/internal/scan"
)

// FormatVersion is bumped whenever the on-disk layout changes.
const FormatVersion = 1

// ErrStale reports an index whose input digest no longer matches.
var ErrStale = errors.New("index: stale index for input digest")

// ErrMissing reports an absent index.
var ErrMissing = errors.New("index: missing index")

// ErrCorrupt reports a structurally invalid index.
var ErrCorrupt = errors.New("index: corrupt index")

// Record is one indexed metadata entry.
type Record struct {
	Kind     meta.Kind      `json:"kind"`
	Track    int            `json:"track"`
	TimeUS   int64          `json:"time_us"`
	Duration int64          `json:"duration_us,omitempty"`
	PTS      int64          `json:"pts"`
	TimeBase clock.Rational `json:"time_base"`
	Text     string         `json:"text,omitempty"`
	Payload  []byte         `json:"payload,omitempty"`
}

// Keyframe is one indexed random-access point.
type Keyframe struct {
	Track   int   `json:"track"`
	TimeUS  int64 `json:"time_us"`
	PTS     int64 `json:"pts"`
	BytePos int64 `json:"byte_pos"`
	Size    int   `json:"size"`
}

// Track is the cached track description.
type Track struct {
	Index      int            `json:"index"`
	Kind       string         `json:"kind"`
	Codec      string         `json:"codec"`
	TimeBase   clock.Rational `json:"time_base"`
	StartUS    int64          `json:"start_us"`
	DurationUS int64          `json:"duration_us"`
	SampleRate int            `json:"sample_rate,omitempty"`
	Channels   int            `json:"channels,omitempty"`
}

// Chapter is a cached chapter position.
type Chapter struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	StartUS int64  `json:"start_us"`
	EndUS   int64  `json:"end_us"`
}

// Index is the serialized scan bound to one input digest.
type Index struct {
	Version    int        `json:"version"`
	InputPath  string     `json:"input_path"`
	InputSize  int64      `json:"input_size"`
	Digest     string     `json:"digest"`
	Format     string     `json:"format"`
	DurationUS int64      `json:"duration_us"`
	TrackList  []Track    `json:"tracks"`
	Keyframes  []Keyframe `json:"keyframes"`
	Records    []Record   `json:"records"`
	Chapters   []Chapter  `json:"chapters"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Summary is the terminal-facing digest of an index.
type Summary struct {
	Digest     string
	Keyframes  int
	Records    int
	Chapters   int
	Tracks     int
	DurationUS int64
}

// Summarize reduces an index to reportable counts.
func (i *Index) Summarize() Summary {
	return Summary{
		Digest: i.Digest, Keyframes: len(i.Keyframes), Records: len(i.Records),
		Chapters: len(i.Chapters), Tracks: len(i.TrackList), DurationUS: i.DurationUS,
	}
}

// Build converts a scan into an index bound to digest.
func Build(s *scan.Scan, digest string, size int64) (*Index, error) {
	if len(digest) != 64 {
		return nil, fmt.Errorf("%w: digest must be 64 hex chars, got %d", ErrCorrupt, len(digest))
	}
	ix := &Index{
		Version: FormatVersion, InputPath: s.Path, InputSize: size, Digest: digest,
		Format: s.Format, DurationUS: s.DurationUS, CreatedAt: time.Now().UTC(),
	}
	for _, t := range s.Tracks {
		startUS := int64(0)
		if s.Timeline != nil {
			if v, err := s.Timeline.ToUnified(t.Index, t.Start); err == nil && v != clock.NoTimestamp {
				startUS = v
			}
		}
		durUS := int64(0)
		if s.Timeline != nil && t.Duration != clock.NoTimestamp {
			if v, err := s.Timeline.Duration(t.Index); err == nil && v != clock.NoTimestamp {
				durUS = v
			}
		}
		ix.TrackList = append(ix.TrackList, Track{
			Index: t.Index, Kind: string(t.Kind), Codec: t.Codec, TimeBase: t.TimeBase,
			StartUS: startUS, DurationUS: durUS, SampleRate: t.SampleRate, Channels: t.Channels,
		})
	}
	for _, k := range s.Keyframes {
		ix.Keyframes = append(ix.Keyframes, Keyframe{
			Track: k.Track, TimeUS: k.TimeUS, PTS: k.PTS, BytePos: k.BytePos, Size: k.Size,
		})
	}
	for _, m := range s.Meta {
		if m.Quarantined {
			// Quarantined records are reported by the scanner, never indexed
			// as usable metadata.
			continue
		}
		ix.Records = append(ix.Records, Record{
			Kind: m.Kind, Track: m.Track, TimeUS: m.TimeUS, Duration: m.Duration,
			PTS: m.PTS, TimeBase: m.TimeBase, Text: m.Text, Payload: m.Payload,
		})
	}
	for _, c := range s.Chapters {
		ix.Chapters = append(ix.Chapters, Chapter{ID: c.ID, Title: c.Title, StartUS: c.StartUs, EndUS: c.EndUs})
	}
	sort.SliceStable(ix.Keyframes, func(a, b int) bool {
		if ix.Keyframes[a].TimeUS != ix.Keyframes[b].TimeUS {
			return ix.Keyframes[a].TimeUS < ix.Keyframes[b].TimeUS
		}
		return ix.Keyframes[a].Track < ix.Keyframes[b].Track
	})
	sort.SliceStable(ix.Records, func(a, b int) bool {
		if ix.Records[a].TimeUS != ix.Records[b].TimeUS {
			return ix.Records[a].TimeUS < ix.Records[b].TimeUS
		}
		return ix.Records[a].Kind < ix.Records[b].Kind
	})
	if err := ix.Validate(); err != nil {
		return nil, err
	}
	return ix, nil
}

// Validate checks structural invariants so a corrupt cache cannot be used.
func (i *Index) Validate() error {
	if i.Version != FormatVersion {
		return fmt.Errorf("%w: version %d", ErrCorrupt, i.Version)
	}
	if len(i.Digest) != 64 {
		return fmt.Errorf("%w: digest length %d", ErrCorrupt, len(i.Digest))
	}
	seen := map[int]bool{}
	for _, t := range i.TrackList {
		if t.Index < 0 || seen[t.Index] {
			return fmt.Errorf("%w: track %d", ErrCorrupt, t.Index)
		}
		seen[t.Index] = true
		if !t.TimeBase.Valid() {
			return fmt.Errorf("%w: track %d time base", ErrCorrupt, t.Index)
		}
		if t.DurationUS < 0 || t.StartUS < 0 {
			return fmt.Errorf("%w: track %d negative position", ErrCorrupt, t.Index)
		}
	}
	for _, k := range i.Keyframes {
		if !seen[k.Track] {
			return fmt.Errorf("%w: keyframe for unknown track %d", ErrCorrupt, k.Track)
		}
		if k.TimeUS < 0 {
			return fmt.Errorf("%w: keyframe negative time", ErrCorrupt)
		}
	}
	for _, r := range i.Records {
		if !seen[r.Track] {
			return fmt.Errorf("%w: record for unknown track %d", ErrCorrupt, r.Track)
		}
		if r.TimeUS < 0 {
			return fmt.Errorf("%w: record negative time", ErrCorrupt)
		}
		if len(r.Payload) > meta.MaxRecordBytes {
			return fmt.Errorf("%w: record payload %d bytes", ErrCorrupt, len(r.Payload))
		}
	}
	for _, c := range i.Chapters {
		if c.EndUS < c.StartUS || c.StartUS < 0 {
			return fmt.Errorf("%w: chapter %d [%d,%d)", ErrCorrupt, c.ID, c.StartUS, c.EndUS)
		}
	}
	return nil
}

// KeyframeBefore returns the newest keyframe at or before t on track.
func (i *Index) KeyframeBefore(track int, t int64) (Keyframe, bool) {
	var best Keyframe
	found := false
	for _, k := range i.Keyframes {
		if k.Track != track || k.TimeUS > t {
			continue
		}
		if !found || k.TimeUS > best.TimeUS {
			best, found = k, true
		}
	}
	return best, found
}

// RecordsBetween returns non-quarantined records in [start, end).
func (i *Index) RecordsBetween(start, end int64) []Record {
	var out []Record
	for _, r := range i.Records {
		if r.TimeUS >= start && r.TimeUS < end {
			out = append(out, r)
		}
	}
	return out
}

// Store is a filesystem cache of checked indexes.
type Store struct {
	Dir string
}

// NewStore creates the cache directory.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("index: empty store dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("index: create store: %w", err)
	}
	return &Store{Dir: dir}, nil
}

// path returns the cache file for a digest.
func (s *Store) path(digest string) string {
	return filepath.Join(s.Dir, digest+".idx.json")
}

// Load reads the cached index for digest and verifies freshness.
func (s *Store) Load(digest string) (*Index, error) {
	b, err := os.ReadFile(s.path(digest))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMissing
		}
		return nil, fmt.Errorf("index: read: %w", err)
	}
	var ix Index
	if err := json.Unmarshal(b, &ix); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if ix.Digest != digest {
		return nil, fmt.Errorf("%w: cached digest %s != %s", ErrStale, ix.Digest, digest)
	}
	if err := ix.Validate(); err != nil {
		return nil, err
	}
	return &ix, nil
}

// Save writes the index atomically (temp file + rename) so a crash cannot
// leave a half-written cache behind.
func (s *Store) Save(ix *Index) error {
	if err := ix.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(ix)
	if err != nil {
		return fmt.Errorf("index: encode: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, ".idx-*")
	if err != nil {
		return fmt.Errorf("index: temp: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("index: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("index: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("index: close: %w", err)
	}
	if err := os.Rename(name, s.path(ix.Digest)); err != nil {
		return fmt.Errorf("index: rename: %w", err)
	}
	return nil
}

// EnsureResult reports how an index was obtained.
type EnsureResult struct {
	Index       *Index
	Rebuilt     bool
	Invalidated bool
	Reason      string
}

// Ensure loads a fresh index, rebuilding when missing, corrupt or stale.
// Invalidation is atomic: the stale file is removed only after the fresh index
// has been written under its new digest.
func (s *Store) Ensure(digest string, size int64, build func() (*scan.Scan, error)) (EnsureResult, error) {
	if old, err := s.Load(digest); err == nil {
		return EnsureResult{Index: old, Reason: "cache hit"}, nil
	} else if !errors.Is(err, ErrMissing) && !errors.Is(err, ErrStale) && !errors.Is(err, ErrCorrupt) {
		return EnsureResult{}, err
	}
	sc, err := build()
	if err != nil {
		return EnsureResult{}, err
	}
	ix, err := Build(sc, digest, size)
	if err != nil {
		return EnsureResult{}, err
	}
	if err := s.Save(ix); err != nil {
		return EnsureResult{}, err
	}
	return EnsureResult{Index: ix, Rebuilt: true, Reason: "rebuilt"}, nil
}

// Invalidate removes the cached entry for a digest.
func (s *Store) Invalidate(digest string) error {
	err := os.Remove(s.path(digest))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("index: invalidate: %w", err)
	}
	return nil
}

// InvalidateStale removes cached indexes for inputPath whose recorded digest
// differs from digest, and reports how many were removed. This is the atomic
// response to "the input changed": old entries must never steer a new run.
func (s *Store) InvalidateStale(inputPath, digest string) (int, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0, fmt.Errorf("index: list for invalidation: %w", err)
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if len(name) != 64+len(".idx.json") || filepath.Ext(name) != ".json" {
			continue
		}
		cached := name[:64]
		if cached == digest {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Dir, name))
		if err != nil {
			continue
		}
		var ix Index
		if err := json.Unmarshal(b, &ix); err != nil {
			continue
		}
		if ix.InputPath != inputPath {
			continue
		}
		if err := os.Remove(filepath.Join(s.Dir, name)); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("index: invalidate stale: %w", err)
		}
		removed++
	}
	return removed, nil
}

// List returns the digests currently cached.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("index: list: %w", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if len(name) == 64+len(".idx.json") && filepath.Ext(name) == ".json" {
			out = append(out, name[:64])
		}
	}
	return out, nil
}
