package manifest

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Load reads a manifest (HLS playlist or DASH MPD) by root-relative path and
// dispatches on its extension and first bytes.
func Load(r *Resolver, relPath string) (*Timeline, error) {
	full, err := r.Resolve(relPath)
	if err != nil {
		return nil, err
	}
	data, err := ReadLimited(full)
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(filepath.Ext(full))
	switch {
	case lower == ".m3u8" || lower == ".m3u":
		return ParseHLS(r, relPath, data)
	case lower == ".mpd":
		return ParseDASH(r, relPath, data)
	}
	head := strings.TrimSpace(string(data))
	if strings.HasPrefix(head, "#EXTM3U") {
		return ParseHLS(r, relPath, data)
	}
	if strings.HasPrefix(head, "<?xml") || strings.Contains(head, "<MPD") {
		return ParseDASH(r, relPath, data)
	}
	return nil, fmt.Errorf("%w: unknown manifest type %s", ErrBadManifest, relPath)
}

// MarkPresence fills Segment.Present for every stream by resolving the URI
// against the workspace root. The first absent segment is reported in
// FirstMiss so downstream stages can name the first missing dependency.
func MarkPresence(r *Resolver, tl *Timeline) error {
	tl.Missing = nil
	tl.FirstMiss = ""
	for si := range tl.Streams {
		for gi := range tl.Streams[si].Segments {
			seg := &tl.Streams[si].Segments[gi]
			full, err := r.Resolve(seg.URI)
			if err != nil {
				return err
			}
			info, statErr := statFile(full)
			if statErr != nil || !info {
				seg.Present = false
				name := seg.URI
				tl.Missing = append(tl.Missing, name)
				if tl.FirstMiss == "" {
					tl.FirstMiss = name
				}
				continue
			}
			seg.Present = true
		}
	}
	return nil
}
