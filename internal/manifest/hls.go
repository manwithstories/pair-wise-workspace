// Package manifest generates synchronized master and media HLS playlists.
package manifest

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Eyevinn/hls-m3u8/m3u8"

	"github.com/pairwise/relay/internal/media"
)

// Segment is one aligned HLS segment in one representation.
type Segment struct {
	Index         uint64
	URI           string
	Start90k      int64
	Duration90k   int64
	Discontinuity bool
	Gap           bool
}

// Rendition describes one bitrate ladder entry and its aligned segments.
type Rendition struct {
	ID         string
	Playlist   string
	Bandwidth  uint32
	Average    uint32
	Codecs     string
	Resolution string
	FrameRate  float64
	Segments   []Segment
}

// Bundle contains generated playlist file contents keyed by relative path.
type Bundle struct {
	MasterFile string
	Files      map[string][]byte
}

// Build validates common boundaries and invokes the Eyevinn m3u8 writer.
func Build(masterFile string, renditions []Rendition) (*Bundle, error) {
	if len(renditions) == 0 {
		return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "at least one rendition is required")
	}
	if masterFile == "" {
		masterFile = "master.m3u8"
	}
	count := len(renditions[0].Segments)
	starts := make([]int64, count)
	for i, s := range renditions[0].Segments {
		starts[i] = s.Start90k
	}

	master := m3u8.NewMasterPlaylist()
	files := make(map[string][]byte, len(renditions)+1)
	seenNames := make(map[string]bool)

	for ri, r := range renditions {
		if r.ID == "" || r.Playlist == "" {
			return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{Rendition: r.ID}, -1, false, "rendition id and playlist name are required")
		}
		if seenNames[r.Playlist] {
			return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{Rendition: r.ID}, -1, false, "duplicate playlist name %s", r.Playlist)
		}
		seenNames[r.Playlist] = true
		if len(r.Segments) != count {
			return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{Rendition: r.ID}, -1, false,
				"segment count %d != first rendition count %d", len(r.Segments), count)
		}

		pl, err := m3u8.NewMediaPlaylist(0, uint(len(r.Segments)))
		if err != nil {
			return nil, err
		}
		pl.SetVersion(7)
		pl.SeqNo = 1
		pl.MediaType = m3u8.VOD
		var target uint
		for i, s := range r.Segments {
			if s.Index != uint64(i+1) {
				return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{Rendition: r.ID, Segment: s.Index}, -1, false, "segment indices must be one-based and ordered")
			}
			if s.Duration90k <= 0 {
				return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{Rendition: r.ID, Segment: s.Index}, -1, false, "duration must be positive")
			}
			if ri > 0 && !media.WithinOneTick(s.Start90k, starts[i]) {
				return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{Rendition: r.ID, Segment: s.Index}, -1, false,
					"start %d differs from common boundary %d by %d ticks", s.Start90k, starts[i], media.AbsTickDiff(s.Start90k, starts[i]))
			}
			if !safeURI(s.URI) {
				return nil, media.NewFault(media.CodeUnsafePath, media.FragmentID{Rendition: r.ID, Segment: s.Index}, -1, false, "unsafe segment URI %q", s.URI)
			}
			duration := media.Seconds(s.Duration90k)
			target = maxUint(target, uint(math.Ceil(duration)))
			seg := &m3u8.MediaSegment{
				URI:           s.URI,
				Duration:      duration,
				Discontinuity: s.Discontinuity,
				Gap:           s.Gap,
			}
			if err := pl.AppendSegment(seg); err != nil {
				return nil, fmt.Errorf("append %s/%d: %w", r.ID, s.Index, err)
			}
		}
		pl.TargetDuration = target
		pl.Close()
		files[r.Playlist] = []byte(pl.String())

		params := m3u8.VariantParams{
			Bandwidth:        r.Bandwidth,
			AverageBandwidth: r.Average,
			Codecs:           orDefault(r.Codecs, "avc1.64001f,mp4a.40.2"),
			Resolution:       r.Resolution,
			FrameRate:        r.FrameRate,
		}
		master.Append(r.Playlist, nil, params)
	}

	files[masterFile] = []byte(master.String())
	return &Bundle{MasterFile: masterFile, Files: files}, nil
}

// SortedNames returns deterministic playlist output order.
func (b *Bundle) SortedNames() []string {
	names := make([]string, 0, len(b.Files))
	for n := range b.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func safeURI(uri string) bool {
	if uri == "" || strings.Contains(uri, "..") || filepath.IsAbs(uri) {
		return false
	}
	clean := filepath.Clean(uri)
	return clean != "." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func maxUint(a, b uint) uint {
	if a > b {
		return a
	}
	return b
}
