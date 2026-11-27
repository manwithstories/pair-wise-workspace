package manifest

import (
	"path/filepath"
	"strings"
)

// Parse dispatches to the HLS or DASH parser based on the master-playlist
// file extension (.m3u8 vs .mpd).
func Parse(camera, masterPath string) (*Manifest, error) {
	ext := strings.ToLower(filepath.Ext(masterPath))
	switch ext {
	case ".m3u8", ".m3u":
		return ParseHLS(camera, masterPath)
	case ".mpd":
		return ParseDASH(camera, masterPath)
	default:
		return nil, ErrInvalidManifest
	}
}

// Representation looks up a rendition by ID, returning nil when absent.
func (m *Manifest) Representation(id string) *Representation {
	for i := range m.Representations {
		if m.Representations[i].ID == id {
			return &m.Representations[i]
		}
	}
	return nil
}

// Segment locates a segment by sequence number within a representation.
func (r *Representation) Segment(seq int) *Segment {
	for i := range r.Segments {
		if r.Segments[i].Sequence == seq {
			return &r.Segments[i]
		}
	}
	return nil
}
