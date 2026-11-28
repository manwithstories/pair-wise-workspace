// Package manifest writes and reads HLS playlists through the
// Eyevinn/hls-m3u8 library: per-variant media playlists produced by the
// segment layer are read back for alignment auditing, the subtitle media
// playlist is generated from sliced VTT cues, and the master playlist ties
// variants and subtitles together. All writes are atomic (temp + rename).
package manifest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Eyevinn/hls-m3u8/m3u8"

	"presspack/internal/core"
)

// VariantInfo describes one rendition for the master playlist.
type VariantInfo struct {
	Name         string // directory name relative to the master
	PlaylistFile string // playlist file name inside the variant dir
	Bandwidth    uint32
	Width        int
	Height       int
	Codecs       string
	FrameRate    float64
}

// SubtitleInfo describes the subtitle rendition group.
type SubtitleInfo struct {
	GroupID  string
	Name     string
	Language string
	URI      string // playlist URI relative to the master
}

// WriteMaster renders the master playlist and atomically replaces path.
func WriteMaster(path string, variants []VariantInfo, subs *SubtitleInfo) error {
	master := m3u8.NewMasterPlaylist()
	for _, v := range variants {
		params := m3u8.VariantParams{
			Bandwidth:        v.Bandwidth,
			AverageBandwidth: v.Bandwidth,
			Codecs:           v.Codecs,
			Resolution:       fmt.Sprintf("%dx%d", v.Width, v.Height),
			FrameRate:        v.FrameRate,
		}
		if subs != nil {
			params.Subtitles = subs.GroupID
			params.Alternatives = []*m3u8.Alternative{{
				Type:       "SUBTITLES",
				GroupId:    subs.GroupID,
				Name:       subs.Name,
				Language:   subs.Language,
				URI:        subs.URI,
				Default:    true,
				Autoselect: true,
			}}
		}
		uri := v.Name + "/" + v.PlaylistFile
		master.Append(uri, nil, params)
	}
	return atomicWrite(path, master.Encode().Bytes())
}

// WriteSubtitlePlaylist builds a VOD media playlist for sliced VTT files.
// names and durations must have equal length; durations are exact ticks.
func WriteSubtitlePlaylist(path string, names []string, durations []core.Time) error {
	if len(names) != len(durations) {
		return fmt.Errorf("subtitle playlist: %d names vs %d durations", len(names), len(durations))
	}
	p, err := m3u8.NewMediaPlaylist(uint(len(names)), uint(len(names)))
	if err != nil {
		return err
	}
	p.MediaType = m3u8.VOD
	for i, n := range names {
		if err := p.Append(n, durations[i].Seconds(), ""); err != nil {
			return err
		}
	}
	p.Close()
	return atomicWrite(path, p.Encode().Bytes())
}

// ReadSegmentDurations decodes a media playlist and returns each segment's
// duration on the unified time base.
func ReadSegmentDurations(path string) ([]core.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pl, listType, err := m3u8.DecodeFrom(bytes.NewReader(raw), true)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if listType != m3u8.MEDIA {
		return nil, fmt.Errorf("%s is not a media playlist", path)
	}
	media := pl.(*m3u8.MediaPlaylist)
	var out []core.Time
	for _, seg := range media.Segments {
		if seg == nil {
			continue
		}
		out = append(out, core.FromMillis(int64(seg.Duration*1000+0.5)))
	}
	return out, nil
}

// atomicWrite stages content in a sibling temp file then renames it over
// the target, so readers never observe a partial playlist.
func atomicWrite(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
