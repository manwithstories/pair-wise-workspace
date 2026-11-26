package hls

import (
	"fmt"
	"strings"

	"github.com/Eyevinn/hls-m3u8/m3u8"

	"deliver/internal/model"
)

// Rendition is one output bitrate ladder rung.
type Rendition struct {
	Name      string
	Bandwidth uint32
}

// BuildMediaPlaylist renders a closed VOD playlist for one rendition
// from aligned segment boundaries.
func BuildMediaPlaylist(r Rendition, segs []Segment) (string, error) {
	p, err := m3u8.NewMediaPlaylist(0, uint(len(segs)))
	if err != nil {
		return "", model.Fatalf(model.StageHLS, "playlist: %v", err)
	}
	p.MediaType = m3u8.VOD
	var maxDur float64
	for _, s := range segs {
		dur := float64(s.EndMs-s.StartMs) / 1000.0
		if err := p.Append(fmt.Sprintf("%s/seg_%05d.ts", r.Name, s.Index), dur, ""); err != nil {
			return "", model.Fatalf(model.StageHLS, "append segment %d: %v", s.Index, err)
		}
		if dur > maxDur {
			maxDur = dur
		}
	}
	p.TargetDuration = uint(maxDur + 0.5)
	p.Close()
	return p.String(), nil
}

// BuildMaster renders the master playlist referencing every rendition.
func BuildMaster(rends []Rendition) string {
	m := m3u8.NewMasterPlaylist()
	for _, r := range rends {
		m.Append(r.Name+"/index.m3u8", nil, m3u8.VariantParams{
			Bandwidth:        r.Bandwidth,
			AverageBandwidth: r.Bandwidth * 9 / 10,
			Codecs:           "avc1.64001f,mp4a.40.2",
		})
	}
	return m.String()
}

// VerifyMediaPlaylist independently re-parses a generated manifest and
// checks segment count and per-segment durations against the aligned
// boundaries, allowing at most one frame of skew.
func VerifyMediaPlaylist(manifest string, segs []Segment, frameMs float64) error {
	pl, lt, err := m3u8.DecodeFrom(strings.NewReader(manifest), true)
	if err != nil {
		return model.Fatalf(model.StageHLS, "re-parse manifest: %v", err)
	}
	if lt != m3u8.MEDIA {
		return model.Fatalf(model.StageHLS, "manifest is not a media playlist")
	}
	mp := pl.(*m3u8.MediaPlaylist)
	n := 0
	for _, s := range mp.Segments {
		if s != nil {
			n++
		}
	}
	if n != len(segs) {
		return model.Fatalf(model.StageHLS, "manifest has %d segments, want %d", n, len(segs))
	}
	i := 0
	for _, s := range mp.Segments {
		if s == nil {
			continue
		}
		want := float64(segs[i].EndMs-segs[i].StartMs) / 1000.0
		if d := s.Duration - want; d > frameMs/1000.0 || d < -frameMs/1000.0 {
			return model.Fatalf(model.StageHLS,
				"segment %d duration %.3fs off by more than one frame", i, s.Duration)
		}
		i++
	}
	if !mp.Closed {
		return model.Fatalf(model.StageHLS, "manifest missing EXT-X-ENDLIST")
	}
	return nil
}
