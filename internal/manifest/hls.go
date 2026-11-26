// Package manifest renders HLS media and master playlists with Eyevinn's
// hls-m3u8 library and records unlocated/located corruption at the edge.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	hls "github.com/Eyevinn/hls-m3u8/m3u8"

	"github.com/relayteam/relay/internal/media"
)

// Segment is one integer-tick HLS segment in a representation playlist.
type Segment struct {
	URI           string `json:"uri"`
	DurationTicks int64  `json:"duration_ticks90k"`
	Title         string `json:"title,omitempty"`
	Discontinuity bool   `json:"discontinuity,omitempty"`
}

// Media is one track/bitrate media playlist.
type Media struct {
	Key        string    `json:"key"`
	File       string    `json:"file"`
	Track      string    `json:"track"`
	Rendition  string    `json:"rendition"`
	Bandwidth  uint32    `json:"bandwidth"`
	Codecs     string    `json:"codecs"`
	Resolution string    `json:"resolution"`
	Segments   []Segment `json:"segments"`
}

// WriteAll writes every media playlist and one master playlist into root.
func WriteAll(root string, medias []Media) (string, []string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", nil, media.E(media.ErrCheckpoint, "manifest.hls", media.Meta{}, err)
	}
	ordered := append([]Media(nil), medias...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })
	var files []string
	for _, m := range ordered {
		if err := validateMedia(m); err != nil {
			return "", files, err
		}
		data, err := encodeMedia(m)
		if err != nil {
			return "", files, err
		}
		path := filepath.Join(root, m.File)
		if err := atomicWrite(path, data); err != nil {
			return "", files, media.E(media.ErrCheckpoint, "manifest.hls", media.Meta{}, err)
		}
		files = append(files, path)
	}
	master, err := encodeMaster(ordered)
	if err != nil {
		return "", files, err
	}
	masterPath := filepath.Join(root, "master.m3u8")
	if err := atomicWrite(masterPath, master); err != nil {
		return "", files, media.E(media.ErrCheckpoint, "manifest.hls", media.Meta{}, err)
	}
	files = append(files, masterPath)
	return masterPath, files, nil
}

func encodeMedia(m Media) ([]byte, error) {
	capacity := uint(len(m.Segments))
	if capacity == 0 {
		capacity = 1
	}
	pl, err := hls.NewMediaPlaylist(0, capacity)
	if err != nil {
		return nil, media.E(media.ErrCheckpoint, "manifest.hls", media.Meta{}, err)
	}
	pl.MediaType = hls.VOD
	var maxTicks int64
	var total int64
	for _, s := range m.Segments {
		if s.DurationTicks > maxTicks {
			maxTicks = s.DurationTicks
		}
		total += s.DurationTicks
		seg := &hls.MediaSegment{
			URI:           s.URI,
			Duration:      media.Tick(s.DurationTicks).Seconds(),
			Title:         s.Title,
			Discontinuity: s.Discontinuity,
		}
		if err := pl.AppendSegment(seg); err != nil {
			return nil, media.E(media.ErrCheckpoint, "manifest.hls", media.Meta{}, err)
		}
	}
	if maxTicks > 0 {
		pl.TargetDuration = uint(math.Ceil(media.Tick(maxTicks).Seconds()))
		if pl.TargetDuration == 0 {
			pl.TargetDuration = 1
		}
	}
	pl.Close()
	buf := pl.Encode()
	out := buf.Bytes()
	if !strings.HasPrefix(string(out), "#EXTM3U") {
		return nil, media.E(media.ErrCheckpoint, "manifest.hls", media.Meta{},
			errors.New("encoded media playlist missing #EXTM3U"))
	}
	_ = total
	return out, nil
}

func encodeMaster(medias []Media) ([]byte, error) {
	pl := hls.NewMasterPlaylist()
	for _, m := range medias {
		params := hls.VariantParams{
			Bandwidth:  m.Bandwidth,
			Codecs:     m.Codecs,
			Resolution: m.Resolution,
		}
		pl.Append(m.File, nil, params)
	}
	return pl.Encode().Bytes(), nil
}

func validateMedia(m Media) error {
	if m.File == "" {
		return media.E(media.ErrArgument, "manifest.hls", media.Meta{},
			fmt.Errorf("media %s has no file", m.Key))
	}
	if len(m.Segments) == 0 {
		return media.E(media.ErrDurationMismatch, "manifest.hls", media.Meta{},
			fmt.Errorf("media %s has no segments", m.Key))
	}
	var last int64
	for i, s := range m.Segments {
		if s.DurationTicks <= 0 {
			return media.E(media.ErrDurationMismatch, "manifest.hls", media.Meta{},
				fmt.Errorf("media %s segment %d has non-positive duration", m.Key, i))
		}
		last += s.DurationTicks
	}
	return nil
}

// CheckEqualDuration ensures all media playlists cover the same tick duration.
func CheckEqualDuration(medias []Media) error {
	if len(medias) == 0 {
		return nil
	}
	var want int64
	for i, m := range medias {
		var d int64
		for _, s := range m.Segments {
			d += s.DurationTicks
		}
		if i == 0 {
			want = d
			continue
		}
		if d != want || len(m.Segments) != len(medias[0].Segments) {
			return media.E(media.ErrDurationMismatch, "manifest.hls", media.Meta{
				Track: m.Track, Rendition: m.Rendition, RecordNo: -1,
			}, fmt.Errorf("%s duration=%d segments=%d, want %d", m.Key, d, len(m.Segments), want))
		}
	}
	return nil
}

// ReportCorruption writes the unified located error at the manifest boundary
// and returns a copy annotated with the manifest stage.
func ReportCorruption(root string, err error) error {
	annotated := media.Annotate(err, "manifest.hls")
	me, ok := media.First(annotated)
	if !ok {
		me = media.E(media.ErrUnlocatedCorrupt, "manifest.hls", media.Meta{RecordNo: -1}, err)
	}
	if mkErr := os.MkdirAll(root, 0o755); mkErr == nil {
		if b, jerr := json.MarshalIndent(me, "", "  "); jerr == nil {
			_ = atomicWrite(filepath.Join(root, "corrupt-record.json"), append(b, '\n'))
		}
	}
	return annotated
}

// AnnotateError propagates an existing unified error through manifest output.
func AnnotateError(err error) error { return media.Annotate(err, "manifest.hls") }

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
