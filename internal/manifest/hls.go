// Package manifest parses local HLS and DASH manifests into a common
// intermediate description. Remote references are rejected.
package manifest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"

	"avrepair/internal/media"
)

// Entry is one manifest segment reference before it is read.
type Entry struct {
	Track         string
	Kind          media.TrackType
	Source        string
	Seq           int
	URI           string
	Path          string
	DeclaredStart media.Rational
	Duration      media.Rational
	Discontinuity bool
	ByteOffset    int64
	ByteLength    int64
}

// Manifest is the parsed manifest description.
type Manifest struct {
	Kind      string
	Source    string
	Timescale int64
	Entries   []Entry
	Warnings  []string
}

// Load detects and parses a manifest file.
func Load(path string) (*Manifest, error) {
	data, err := media.ReadManifest(path)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(data)
	switch {
	case bytes.HasPrefix(trimmed, []byte("#EXTM3U")):
		return ParseHLS(path, data)
	case bytes.HasPrefix(trimmed, []byte("<?xml")) || bytes.Contains(trimmed, []byte("<MPD")):
		return ParseDASH(path, data)
	default:
		return nil, &media.FaultError{Kind: media.ErrNoManifest, Detail: filepath.Base(path)}
	}
}

// localPath validates a segment reference and resolves it under dir.
func localPath(dir, uri string) (string, error) {
	return media.ResolveLocalURI(dir, uri)
}

// parseExtinfDuration converts an EXTINF string into an exact rational.
func parseExtinfDuration(v float64) media.Rational {
	if v <= 0 {
		return media.Zero
	}
	// EXTINF is decimal textual input; recover milliseconds exactly.
	millis := int64(v*1000 + 0.5)
	return media.NewRational(millis, 1000)
}

// HLSResult is re-exported to keep callers independent of the m3u8 package.
type hlsPlaylist struct {
	Media   *m3u8.MediaPlaylist
	Variant string
}

// loadHLSDirectory loads a master playlist and the media playlists it
// references, or a single media playlist.
func loadHLSDirectory(path string, data []byte, kind string) ([]hlsPlaylist, error) {
	pl, listType, err := m3u8.DecodeWith(bytes.NewReader(data), true, nil)
	if err != nil {
		return nil, fmt.Errorf("hls: decode %s: %w", filepath.Base(path), err)
	}
	dir := filepath.Dir(path)
	switch listType {
	case m3u8.MEDIA:
		mediaPL, ok := pl.(*m3u8.MediaPlaylist)
		if !ok {
			return nil, fmt.Errorf("hls: unexpected playlist type %T", pl)
		}
		return []hlsPlaylist{{Media: mediaPL, Variant: ""}}, nil
	case m3u8.MASTER:
		master, ok := pl.(*m3u8.MasterPlaylist)
		if !ok {
			return nil, fmt.Errorf("hls: unexpected playlist type %T", pl)
		}
		var out []hlsPlaylist
		for _, v := range master.Variants {
			if v == nil || v.URI == "" {
				continue
			}
			ref, err := localPath(dir, v.URI)
			if err != nil {
				return nil, err
			}
			raw, err := media.ReadManifest(ref)
			if err != nil {
				return nil, err
			}
			sub, lt, err := m3u8.DecodeWith(bytes.NewReader(raw), true, nil)
			if err != nil {
				return nil, fmt.Errorf("hls: decode variant %s: %w", v.URI, err)
			}
			if lt != m3u8.MEDIA {
				return nil, fmt.Errorf("hls: variant %s is not a media playlist", v.URI)
			}
			out = append(out, hlsPlaylist{Media: sub.(*m3u8.MediaPlaylist), Variant: v.URI})
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("hls: master playlist has no usable variants")
		}
		return out, nil
	default:
		return nil, fmt.Errorf("hls: unknown playlist type")
	}
}

// trackIDFromName derives a stable track id from a variant URI, falling
// back to the manifest base name for single-playlist inputs.
func trackIDFromName(name, kind string) string {
	base := filepath.Base(name)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" || base == "." {
		base = kind
	}
	var b strings.Builder
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "track"
	}
	return b.String()
}

// inferKind guesses a track type from a filename.
func inferKind(name string) media.TrackType {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "video") || strings.Contains(lower, "vid") || strings.Contains(lower, "v_"):
		return media.TrackVideo
	case strings.Contains(lower, "audio") || strings.Contains(lower, "aud") || strings.Contains(lower, "a_"):
		return media.TrackAudio
	default:
		return media.TrackOther
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func statExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
