package manifest

import (
	"fmt"
	"path/filepath"
	"strings"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"

	"avrepair/internal/media"
)

// ParseHLS parses an HLS master or media playlist plus its local variants.
func ParseHLS(path string, data []byte) (*Manifest, error) {
	pls, err := loadHLSDirectory(path, data, "hls")
	if err != nil {
		return nil, err
	}
	out := &Manifest{Kind: "hls", Source: filepath.Clean(path)}
	dir := filepath.Dir(path)
	for _, p := range pls {
		if p.Media == nil {
			continue
		}
		name := p.Variant
		if name == "" {
			name = filepath.Base(path)
		}
		track := trackIDFromName(name, "hls")
		kind := inferKind(name)
		entries, warnings, err := hlsEntries(p.Media, track, kind, dir, filepath.Base(name))
		if err != nil {
			return nil, err
		}
		out.Warnings = append(out.Warnings, warnings...)
		out.Entries = append(out.Entries, entries...)
	}
	if len(out.Entries) == 0 {
		return nil, fmt.Errorf("hls: %s has no media segments", filepath.Base(path))
	}
	return out, nil
}

func hlsEntries(pl *m3u8.MediaPlaylist, track string, kind media.TrackType, dir, name string) ([]Entry, []string, error) {
	var out []Entry
	var warnings []string
	start := media.Zero
	for i, seg := range pl.Segments {
		if seg == nil {
			continue
		}
		if seg.Gap {
			warnings = append(warnings, fmt.Sprintf("%s: gap segment %d skipped", name, i))
			start = start.Add(parseExtinfDuration(seg.Duration))
			continue
		}
		target, err := localPath(dir, seg.URI)
		if err != nil {
			return nil, nil, err
		}
		dur := parseExtinfDuration(seg.Duration)
		if dur.Cmp(media.Zero) == 0 {
			dur = media.NewRational(1, 1)
			warnings = append(warnings, fmt.Sprintf("%s: segment %d has no duration", name, i))
		}
		if seg.Limit < 0 || seg.Offset < 0 {
			return nil, nil, fmt.Errorf("hls: segment %d has negative byte range", i)
		}
		out = append(out, Entry{
			Track:         track,
			Kind:          kind,
			Source:        "hls",
			Seq:           i,
			URI:           seg.URI,
			Path:          target,
			DeclaredStart: start,
			Duration:      dur,
			Discontinuity: seg.Discontinuity,
			ByteOffset:    seg.Offset,
			ByteLength:    seg.Limit,
		})
		start = start.Add(dur)
	}
	return out, warnings, nil
}

// HLSPlaylistName returns a deterministic playlist file name for a track.
func HLSPlaylistName(track string) string { return strings.TrimSpace(track) + ".m3u8" }
