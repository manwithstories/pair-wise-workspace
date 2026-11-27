package hls

import (
	"fmt"
	"path/filepath"

	"github.com/Eyevinn/hls-m3u8/m3u8"
)

// PlaylistSet is the fully resolved set of files for one publication.
type PlaylistSet struct {
	Master      *m3u8.MasterPlaylist
	MediaByName map[string]*m3u8.MediaPlaylist
	// FileName maps a playlist's logical name to its on-disk file name.
	FileName map[string]string
}

// MediaRendition describes an EXT-X-MEDIA audio entry for the master.
type MediaRendition struct {
	GroupID    string
	Name       string
	URI        string
	SampleRate uint32
	Channels   int
	Default    bool
}

// BuildPlaylists turns aligned bounds plus per-variant metadata into a
// master playlist and one media playlist per variant. Output is built in
// memory so the caller can publish every file atomically.
func BuildPlaylists(variants []Variant, bounds []Bound, audio []MediaRendition) (*PlaylistSet, error) {
	master := m3u8.NewMasterPlaylist()
	media := map[string]*m3u8.MediaPlaylist{}
	names := map[string]string{}

	alts := buildAlternatives(audio)

	for _, v := range variants {
		pl, err := m3u8.NewMediaPlaylist(0, uint(len(bounds))+1)
		if err != nil {
			return nil, fmt.Errorf("media playlist %s: %w", v.Name, err)
		}
		pl.Closed = true
		pl.MediaType = m3u8.VOD
		pl.SeqNo = 0
		target := uint(0)
		for _, b := range bounds {
			uri := v.SegmentFile(b.Seq)
			dur := float64(b.End-b.Start) / float64(TicksPerSecond())
			if err := pl.Append(uri, dur, ""); err != nil {
				return nil, err
			}
			if whole := uint(dur + 0.5); whole > target {
				target = whole
			}
		}
		pl.TargetDuration = target
		mediaName := fmt.Sprintf("media-%s.m3u8", v.Name)
		names[v.Name] = mediaName
		media[v.Name] = pl

		params := m3u8.VariantParams{
			Bandwidth:  v.Bandwidth,
			Codecs:     v.Codecs,
			Resolution: v.Resolution,
			FrameRate:  30,
		}
		if len(audio) > 0 {
			params.Audio = audio[0].GroupID
			params.Alternatives = alts
		}
		master.Append(mediaName, pl, params)
	}
	return &PlaylistSet{Master: master, MediaByName: media, FileName: names}, nil
}

func buildAlternatives(audio []MediaRendition) []*m3u8.Alternative {
	var alts []*m3u8.Alternative
	for _, a := range audio {
		alts = append(alts, &m3u8.Alternative{
			Type:       "AUDIO",
			GroupId:    a.GroupID,
			Name:       a.Name,
			URI:        a.URI,
			SampleRate: a.SampleRate,
			Default:    a.Default,
			Autoselect: true,
			Channels:   &m3u8.Channels{Amount: a.Channels},
		})
	}
	return alts
}

// EncodeMaster renders the master playlist to bytes.
func (p *PlaylistSet) EncodeMaster() []byte {
	return p.Master.Encode().Bytes()
}

// EncodeMedia renders one variant's media playlist to bytes.
func (p *PlaylistSet) EncodeMedia(name string) ([]byte, error) {
	pl, ok := p.MediaByName[name]
	if !ok {
		return nil, fmt.Errorf("unknown variant %q", name)
	}
	return pl.Encode().Bytes(), nil
}

// SegmentURIs returns the deterministic, sorted list of segment file
// names referenced across all media playlists.
func (p *PlaylistSet) SegmentURIs() []string {
	seen := map[string]bool{}
	var out []string
	for _, pl := range p.MediaByName {
		for _, seg := range pl.Segments {
			if seg != nil && !seen[seg.URI] {
				seen[seg.URI] = true
				out = append(out, seg.URI)
			}
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// JoinPath is a small helper kept here so callers build file paths
// consistently with filepath semantics.
func JoinPath(dir, name string) string { return filepath.Join(dir, name) }
