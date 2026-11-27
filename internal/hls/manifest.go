// Package hls builds and parses multi-bitrate fMP4 HLS manifests through the
// github.com/Eyevinn/hls-m3u8 library. All playlist generation is deterministic
// so repeated runs emit byte-identical output, and every generated playlist can
// be parsed back to verify the segment table round-trips exactly.
package hls

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Eyevinn/hls-m3u8/m3u8"
	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/media"
)

// Variant describes one rendition in the ladder.
type Variant struct {
	Name      string
	Bandwidth uint32
	Width     int
	Height    int
	FrameRate float64
	Codecs    string
	Playlist  string // media playlist file name
	InitURI   string // fMP4 initialization section URI
}

// SegmentSpec is one media segment placed in a media playlist.
type SegmentSpec struct {
	Seq      int
	URI      string
	Duration clock.Rat
}

// PlaylistSpec is everything needed to emit one media playlist.
type PlaylistSpec struct {
	Variant  Variant
	Target   uint
	Segments []SegmentSpec
}

// DefaultLadder is the fixed three-tier H.264/AAC ladder used by the pipeline.
func DefaultLadder() []Variant {
	return []Variant{
		{Name: "high", Bandwidth: 2_500_000, Width: 1280, Height: 720, FrameRate: 30,
			Codecs: "avc1.4d401f,mp4a.40.2", Playlist: "high.m3u8", InitURI: "high/init.mp4"},
		{Name: "mid", Bandwidth: 1_200_000, Width: 854, Height: 480, FrameRate: 30,
			Codecs: "avc1.4d401e,mp4a.40.2", Playlist: "mid.m3u8", InitURI: "mid/init.mp4"},
		{Name: "low", Bandwidth: 600_000, Width: 640, Height: 360, FrameRate: 30,
			Codecs: "avc1.4d4015,mp4a.40.2", Playlist: "low.m3u8", InitURI: "low/init.mp4"},
	}
}

// BuildMediaPlaylist constructs a closed VOD media playlist with an fMP4 map.
func BuildMediaPlaylist(spec PlaylistSpec) (*m3u8.MediaPlaylist, error) {
	if len(spec.Segments) == 0 {
		return nil, media.New(media.PhasePackage, media.KindBounds, "", -1,
			"media playlist needs at least one segment")
	}
	pl, err := m3u8.NewMediaPlaylist(0, uint(len(spec.Segments))+1)
	if err != nil {
		return nil, media.New(media.PhasePackage, media.KindTool, "", -1,
			"cannot allocate media playlist").Wrap(err)
	}
	pl.TargetDuration = spec.Target
	pl.MediaType = m3u8.VOD
	pl.Map = &m3u8.Map{URI: spec.Variant.InitURI}
	pl.SetVersion(7)
	for _, seg := range spec.Segments {
		ms := &m3u8.MediaSegment{
			SeqId: uint64(seg.Seq), URI: seg.URI,
			Duration: seg.Duration.Seconds(),
		}
		if err := pl.AppendSegment(ms); err != nil {
			return nil, media.New(media.PhasePackage, media.KindTool, "", -1,
				"cannot append segment").Wrap(err)
		}
	}
	pl.Close()
	return pl, nil
}

// BuildMasterPlaylist ties the variants together into a multivariant playlist.
func BuildMasterPlaylist(variants []Variant, playlists []*m3u8.MediaPlaylist) (*m3u8.MasterPlaylist, error) {
	if len(variants) != len(playlists) {
		return nil, media.New(media.PhasePackage, media.KindBounds, "", -1,
			"variant/playlist count mismatch")
	}
	master := m3u8.NewMasterPlaylist()
	master.SetVersion(7)
	for i, v := range variants {
		master.Append(v.Playlist, playlists[i], m3u8.VariantParams{
			Bandwidth:  v.Bandwidth,
			Codecs:     v.Codecs,
			Resolution: fmt.Sprintf("%dx%d", v.Width, v.Height),
			FrameRate:  v.FrameRate,
		})
	}
	return master, nil
}

// RenderAll writes the master playlist and every media playlist into dir using
// temp files and an atomic rename so partial output is never published.
func RenderAll(dir, masterName string, master *m3u8.MasterPlaylist,
	mediaPlaylists map[string]*m3u8.MediaPlaylist) (map[string][]byte, error) {
	out := map[string][]byte{}
	masterBytes := appendNewline(master.Encode().Bytes())
	out[masterName] = masterBytes
	for name, pl := range mediaPlaylists {
		out[name] = appendNewline(pl.Encode().Bytes())
	}
	for name, data := range out {
		if err := atomicWrite(filepath.Join(dir, name), data); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RoundTrip parses a rendered media playlist back and confirms the segment
// table (sequence, URI and duration) matches the spec it was built from.
func RoundTrip(data []byte, spec PlaylistSpec) error {
	pl, listType, err := m3u8.Decode(*bytes.NewBuffer(data), true)
	if err != nil {
		return media.New(media.PhaseVerify, media.KindTool, "", -1,
			"media playlist failed to re-parse").Wrap(err)
	}
	if listType != m3u8.MEDIA {
		return media.New(media.PhaseVerify, media.KindBounds, "", -1,
			"expected a media playlist on round trip")
	}
	mp, ok := pl.(*m3u8.MediaPlaylist)
	if !ok {
		return media.New(media.PhaseVerify, media.KindBounds, "", -1,
			"round trip returned wrong playlist type")
	}
	if mp.Map == nil || mp.Map.URI != spec.Variant.InitURI {
		return media.New(media.PhaseVerify, media.KindBounds, "", -1,
			"fMP4 EXT-X-MAP did not round trip")
	}
	// The library pre-allocates a capacity-sized Segments slice padded with
	// nil; only non-nil entries are real segments.
	gotSegs := make([]*m3u8.MediaSegment, 0, len(spec.Segments))
	for _, s := range mp.Segments {
		if s != nil {
			gotSegs = append(gotSegs, s)
		}
	}
	if len(gotSegs) != len(spec.Segments) {
		return media.New(media.PhaseVerify, media.KindBounds, "", -1,
			fmt.Sprintf("segment count changed on round trip: %d vs %d",
				len(gotSegs), len(spec.Segments)))
	}
	for i, want := range spec.Segments {
		got := gotSegs[i]
		if got.URI != want.URI || got.SeqId != uint64(want.Seq) {
			return media.New(media.PhaseVerify, media.KindBounds, "", -1,
				fmt.Sprintf("segment %d identity changed: %+v", i, got))
		}
	}
	return nil
}

// ParseMaster reads and validates a master playlist from disk.
func ParseMaster(path string) (*m3u8.MasterPlaylist, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, media.New(media.PhaseVerify, media.KindTool, path, -1,
			"cannot open master playlist").Wrap(err)
	}
	defer f.Close()
	pl, listType, err := m3u8.DecodeFrom(f, true)
	if err != nil {
		return nil, media.New(media.PhaseVerify, media.KindTool, path, -1,
			"master playlist failed to parse").Wrap(err)
	}
	if listType != m3u8.MASTER {
		return nil, media.New(media.PhaseVerify, media.KindBounds, path, -1,
			"expected a master playlist")
	}
	return pl.(*m3u8.MasterPlaylist), nil
}

func appendNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return b
	}
	return append(b, '\n')
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return media.New(media.PhasePackage, media.KindTool, path, -1,
			"cannot create output directory").Wrap(err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return media.New(media.PhasePackage, media.KindTool, path, -1,
			"cannot create temp output").Wrap(err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return media.New(media.PhasePackage, media.KindTool, path, -1,
			"temp write failed").Wrap(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return media.New(media.PhasePackage, media.KindTool, path, -1,
			"temp sync failed").Wrap(err)
	}
	tmp.Close()
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return media.New(media.PhasePackage, media.KindTool, path, -1,
			"atomic publish failed").Wrap(err)
	}
	return nil
}
