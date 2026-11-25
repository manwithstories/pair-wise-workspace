// Package manifest renders deterministic HLS master and media playlists.
package manifest

import (
	"fmt"
	"path/filepath"
	"strings"

	"mediad/internal/verify"
)

// Segment is one committed CMAF segment reference.
type Segment struct {
	Index      int
	File       string
	InitFile   string
	DurationMS int64
	ByteSize   int
	SHA256     string
	CutPTS     int64
	SafeCut    bool
}

// Rendition is one bitrate representation.
type Rendition struct {
	ID        string
	Bandwidth int
	Width     int
	Height    int
	Codecs    string
	Playlist  string
	InitFile  string
	Segments  []Segment
}

// Master is the deterministic top-level HLS manifest.
type Master struct {
	Renditions []Rendition
}

// MasterText renders the master playlist. The output intentionally has no
// timestamps or map iteration so its SHA-256 is reproducible.
func (m Master) MasterText() []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	for _, r := range m.Renditions {
		codecs := r.Codecs
		if codecs == "" {
			codecs = "avc1.42E01E,mp4a.40.2"
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=%q", r.Bandwidth, codecs)
		if r.Width > 0 && r.Height > 0 {
			fmt.Fprintf(&b, ",RESOLUTION=%dx%d", r.Width, r.Height)
		}
		b.WriteByte('\n')
		playlist := r.Playlist
		if playlist == "" {
			playlist = filepath.Join(r.ID, "index.m3u8")
		}
		b.WriteString(playlist)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// MediaText renders one VOD media playlist.
func (r Rendition) MediaText() []byte {
	target := int64(1)
	for _, s := range r.Segments {
		if ms := (s.DurationMS + 999) / 1000; ms > target {
			target = ms
		}
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", target)
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	init := r.InitFile
	if init == "" {
		init = "init.mp4"
	}
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", init)
	for _, s := range r.Segments {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", float64(s.DurationMS)/1000)
		file := s.File
		if file == "" {
			file = fmt.Sprintf("seg_%04d.m4s", s.Index)
		}
		b.WriteString(file)
		b.WriteByte('\n')
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String())
}

// Write atomically writes master and all media playlists under dir.
func (m Master) Write(dir string) (string, error) {
	masterPath := filepath.Join(dir, "master.m3u8")
	for _, r := range m.Renditions {
		playlistRel := r.Playlist
		if playlistRel == "" {
			playlistRel = filepath.Join(r.ID, "index.m3u8")
		}
		mediaPath := filepath.Join(dir, playlistRel)
		if err := verify.AtomicWriteFile(mediaPath, r.MediaText(), 0o644); err != nil {
			return "", err
		}
	}
	if err := verify.AtomicWriteFile(masterPath, m.MasterText(), 0o644); err != nil {
		return "", err
	}
	return masterPath, nil
}

// SHA256 returns the hash of the rendered master manifest.
func (m Master) SHA256() string {
	return verify.SHA256Bytes(m.MasterText())
}
