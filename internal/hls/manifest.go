// Package hls builds and round-trips the HLS manifest set for a restored
// program: one master playlist referencing each bitrate rendition and one
// media playlist per rendition listing its segments. Segments flagged with a
// discontinuity carry EXT-X-DISCONTINUITY, and every playlist is written and
// re-parsed with the Eyevinn m3u8 library to prove a standards-readable result.
package hls

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Eyevinn/hls-m3u8/m3u8"

	"github.com/archive/tsrestore/internal/segment"
)

// Variant describes one rendition's playlist parameters.
type Variant struct {
	Name      string
	Bandwidth uint32
	Width     int
	Height    int
}

// BuildMasterPlaylist creates the master playlist referencing every variant.
func BuildMasterPlaylist(variants []Variant) *m3u8.MasterPlaylist {
	master := m3u8.NewMasterPlaylist()
	for _, v := range variants {
		// Media playlists are written as standalone files, so no in-memory
		// chunklist is attached to the master variant.
		master.Append(v.Name+"/index.m3u8", nil, m3u8.VariantParams{
			Bandwidth:  v.Bandwidth,
			Codecs:     "avc1.4D401E",
			Resolution: fmt.Sprintf("%dx%d", v.Width, v.Height),
			Name:       v.Name,
		})
	}
	return master
}

// BuildMediaPlaylist creates a VOD media playlist from ordered segments. The
// playlist capacity is sized to the segment count and the playlist is closed
// with an ENDLIST tag. Discontinuity segments set the appropriate flag.
func BuildMediaPlaylist(segs []*segment.Segment) (*m3u8.MediaPlaylist, error) {
	pl, err := m3u8.NewMediaPlaylist(uint(len(segs)), uint(len(segs))+1)
	if err != nil {
		return nil, err
	}
	for _, s := range segs {
		if err := pl.Append(s.ID+".ts", s.DurationSecs, ""); err != nil {
			return nil, err
		}
		if s.Discontinuity {
			if err := pl.SetDiscontinuity(); err != nil {
				return nil, err
			}
		}
	}
	pl.Close()
	return pl, nil
}

// WriteManifests emits the master and per-rendition playlists under outDir and
// returns the absolute master path. Each rendition's playlist goes in its own
// subdirectory matching the segment layout.
func WriteManifests(outDir string, variants []Variant,
	segments map[string][]*segment.Segment) (string, error) {

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	master := BuildMasterPlaylist(variants)
	masterPath := filepath.Join(outDir, "master.m3u8")
	if err := os.WriteFile(masterPath, master.Encode().Bytes(), 0o644); err != nil {
		return "", err
	}
	for _, v := range variants {
		pl, err := BuildMediaPlaylist(segments[v.Name])
		if err != nil {
			return "", err
		}
		dir := filepath.Join(outDir, v.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		path := filepath.Join(dir, "index.m3u8")
		if err := os.WriteFile(path, pl.Encode().Bytes(), 0o644); err != nil {
			return "", err
		}
	}
	return masterPath, nil
}

// ReadMediaPlaylist parses a written media playlist back, proving the on-disk
// manifest is standards-readable and structurally complete.
func ReadMediaPlaylist(path string) (*m3u8.MediaPlaylist, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pl, listType, err := m3u8.DecodeFrom(f, true)
	if err != nil {
		return nil, err
	}
	if listType != m3u8.MEDIA {
		return nil, fmt.Errorf("expected media playlist, got type %v", listType)
	}
	media, ok := pl.(*m3u8.MediaPlaylist)
	if !ok {
		return nil, fmt.Errorf("decoded playlist is not a media playlist")
	}
	return media, nil
}

// ReadMasterPlaylist parses a written master playlist back.
func ReadMasterPlaylist(path string) (*m3u8.MasterPlaylist, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pl, listType, err := m3u8.DecodeFrom(f, true)
	if err != nil {
		return nil, err
	}
	if listType != m3u8.MASTER {
		return nil, fmt.Errorf("expected master playlist, got type %v", listType)
	}
	master, ok := pl.(*m3u8.MasterPlaylist)
	if !ok {
		return nil, fmt.Errorf("decoded playlist is not a master playlist")
	}
	return master, nil
}
