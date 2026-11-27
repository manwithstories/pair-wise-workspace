// Package playlist renders HLS media and master playlists from measured
// segment durations using the Eyevinn hls-m3u8 library, and parses them
// back for auditing.
package playlist

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"

	"aircheck/internal/clock"
)

// Segment is one HLS media segment with its declared duration.
type Segment struct {
	URI           string `json:"uri"`
	DurationTicks int64  `json:"duration_ticks"` // 90 kHz
}

// Variant describes one rendition in the master playlist.
type Variant struct {
	Name       string `json:"name"`
	URI        string `json:"uri"` // media playlist path relative to master
	Bandwidth  uint32 `json:"bandwidth"`
	Resolution string `json:"resolution,omitempty"`
	Codecs     string `json:"codecs,omitempty"`
}

// WriteMediaPlaylist writes name/playlist.m3u8 under dir atomically.
// Durations are exact tick-derived values so the playlist timeline
// matches the shared cut plan within 1 ms.
func WriteMediaPlaylist(dir string, segs []Segment) error {
	if len(segs) == 0 {
		return fmt.Errorf("no segments to write")
	}
	p, err := m3u8.NewMediaPlaylist(uint(len(segs)), uint(len(segs)))
	if err != nil {
		return err
	}
	for _, s := range segs {
		if err := p.Append(s.URI, clock.Seconds(s.DurationTicks), ""); err != nil {
			return err
		}
	}
	p.Close()
	return atomicWrite(filepath.Join(dir, "playlist.m3u8"), p.Encode().Bytes())
}

// WriteMaster writes the multivariant playlist referencing each variant.
func WriteMaster(dir string, vars []Variant) error {
	m := m3u8.NewMasterPlaylist()
	for _, v := range vars {
		m.Append(v.URI, nil, m3u8.VariantParams{
			Bandwidth:  v.Bandwidth,
			Resolution: v.Resolution,
			Codecs:     v.Codecs,
			Name:       v.Name,
		})
	}
	return atomicWrite(filepath.Join(dir, "master.m3u8"), m.Encode().Bytes())
}

// ReadMediaSegments parses a media playlist and returns segment URIs and
// declared durations in 90 kHz ticks.
func ReadMediaSegments(path string) ([]Segment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pl, listType, err := m3u8.Decode(*bytes.NewBuffer(raw), true)
	if err != nil {
		return nil, fmt.Errorf("playlist %s undecodable: %w", path, err)
	}
	if listType != m3u8.MEDIA {
		return nil, fmt.Errorf("playlist %s is not a media playlist", path)
	}
	mp := pl.(*m3u8.MediaPlaylist)
	var out []Segment
	for _, s := range mp.Segments {
		if s == nil {
			continue
		}
		out = append(out, Segment{URI: s.URI, DurationTicks: clock.FromMillis(s.Duration * 1000)})
	}
	return out, nil
}

// atomicWrite writes via a temp file + rename so interrupted runs never
// leave a torn playlist behind.
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
