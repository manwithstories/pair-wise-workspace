// Package ingest scans on-disk segment files for per-segment metadata
// (timescale, first-frame PTS, duration) and synthesises deterministic
// fixture segments when no external input is provided. Only metadata is
// handled; no decoding is performed.
package ingest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrCorruptSegment marks a segment file whose metadata header cannot be
// parsed.
var ErrCorruptSegment = errors.New("ingest: corrupt segment header")

// SegmentMeta is the probed metadata of one segment. Timescale may change
// mid-stream, so it is carried per segment rather than per track.
type SegmentMeta struct {
	Name      string
	Number    int64
	Timescale int64
	PTS       int64
	Duration  int64
	Bitrate   int64
}

// Track is one probed rendition: an ordered segment list plus ladder
// attributes.
type Track struct {
	ID          string
	SrcDir      string
	StartNumber int64
	Bandwidth   int64
	Width       int
	Height      int
	Segments    []SegmentMeta
}

// ParseHeader decodes the synthetic metadata header line
// "DPK1 number=N ts=T pts=P dur=D br=B".
func ParseHeader(line string) (SegmentMeta, error) {
	var m SegmentMeta
	var tag string
	n, err := fmt.Sscanf(line, "%s number=%d ts=%d pts=%d dur=%d br=%d",
		&tag, &m.Number, &m.Timescale, &m.PTS, &m.Duration, &m.Bitrate)
	if err != nil || n != 6 || tag != "DPK1" {
		return m, ErrCorruptSegment
	}
	if m.Timescale <= 0 || m.Duration <= 0 || m.PTS < 0 {
		return m, ErrCorruptSegment
	}
	return m, nil
}

// ProbeDir scans one rendition directory of *.m4s files and returns the
// track sorted by segment number.
func ProbeDir(dir string) (*Track, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("ingest: read dir %s: %w", dir, err)
	}
	t := &Track{ID: filepath.Base(dir), SrcDir: dir}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".m4s") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("ingest: read %s: %w", e.Name(), err)
		}
		line, _, _ := strings.Cut(string(data), "\n")
		m, err := ParseHeader(line)
		if err != nil {
			return nil, fmt.Errorf("ingest: %s: %w", e.Name(), err)
		}
		m.Name = e.Name()
		t.Segments = append(t.Segments, m)
	}
	if len(t.Segments) == 0 {
		return nil, fmt.Errorf("ingest: no segments in %s", dir)
	}
	sort.Slice(t.Segments, func(i, j int) bool { return t.Segments[i].Number < t.Segments[j].Number })
	t.StartNumber = t.Segments[0].Number
	t.Bandwidth = t.Segments[0].Bitrate
	return t, nil
}

// ProbeMulti probes every immediate subdirectory of root as one rendition
// and assigns ladder geometry by ascending bandwidth.
func ProbeMulti(root string) ([]Track, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("ingest: read dir %s: %w", root, err)
	}
	var tracks []Track
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := ProbeDir(filepath.Join(root, e.Name()))
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, *t)
	}
	if len(tracks) == 0 {
		return nil, fmt.Errorf("ingest: no rendition directories in %s", root)
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].Bandwidth < tracks[j].Bandwidth })
	AssignGeometry(tracks)
	return tracks, nil
}

// AssignGeometry fills Width/Height from the ladder by bandwidth rank.
func AssignGeometry(tracks []Track) {
	for i := range tracks {
		h := LadderHeight(i)
		tracks[i].Height = h
		tracks[i].Width = h * 16 / 9
	}
}

// LadderHeight maps a bandwidth rank to a picture height.
func LadderHeight(rank int) int {
	ladder := []int{360, 480, 720, 1080}
	if rank < len(ladder) {
		return ladder[rank]
	}
	return ladder[len(ladder)-1]
}
