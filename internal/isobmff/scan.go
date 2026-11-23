// Package isobmff parses MP4/MOV sample tables, edit lists and metadata,
// and locates corruption down to a single sample or byte offset.
package isobmff

import (
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"

	"archivecheck/internal/clock"
)

// Limits bound a single asset (24 h duration, 32 tracks).
type Limits struct {
	MaxDurationSec int64
	MaxTracks      int
}

// DefaultLimits are the archive ingest limits.
var DefaultLimits = Limits{MaxDurationSec: 24 * 3600, MaxTracks: 32}

// ColorInfo carries nclx colour metadata of a video track.
type ColorInfo struct {
	Primaries uint16 `json:"primaries"`
	Transfer  uint16 `json:"transfer"`
	Matrix    uint16 `json:"matrix"`
	FullRange bool   `json:"full_range"`
}

// Track holds the raw parsed tables of one track.
type Track struct {
	Number        int    `json:"number"` // 1-based order in file
	TrackID       uint32 `json:"track_id"`
	Handler       string `json:"handler"` // "vide" or "soun"
	Timescale     int64  `json:"timescale"`
	MediaDuration int64  `json:"media_duration"` // mdhd duration, media units

	Stts         []mp4.SttsEntry `json:"-"`
	SampleSizes  []uint32        `json:"-"`
	SyncSamples  []uint32        `json:"-"` // empty: every sample is a keyframe
	ChunkOffsets []uint32        `json:"-"`
	Stsc         []mp4.StscEntry `json:"-"`
	Edits        []clock.Edit    `json:"edits,omitempty"`

	Channels   int `json:"channels,omitempty"`
	SampleRate int `json:"sample_rate,omitempty"`
	Width      int `json:"width,omitempty"`
	Height     int `json:"height,omitempty"`

	Color *ColorInfo `json:"color,omitempty"`

	// Byte offsets of the source boxes, for fault location.
	OffStts int64 `json:"-"`
	OffStsz int64 `json:"-"`
	OffStco int64 `json:"-"`
	OffElst int64 `json:"-"`
}

// IsVideo / IsAudio classify the track by handler type.
func (t *Track) IsVideo() bool { return t.Handler == "vide" }
func (t *Track) IsAudio() bool { return t.Handler == "soun" }

// File is a parsed MP4 asset.
type File struct {
	Path           string   `json:"path"`
	Size           int64    `json:"size"`
	MovieTimescale int64    `json:"movie_timescale"`
	Tracks         []*Track `json:"tracks"`
}

// Read parses the box tree of an MP4/MOV file. It performs no validation;
// corrupt tables are reported by BuildIndexes with precise positions.
func Read(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	out := &File{Path: path, Size: st.Size(), MovieTimescale: 1000}
	var cur *Track

	_, err = mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		depth := len(h.Path)
		inTrak := cur != nil && depth >= 2 && h.Path[1] == mp4.BoxTypeTrak()
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoov():
			return h.Expand()
		case mp4.BoxTypeTrak():
			cur = &Track{Number: len(out.Tracks) + 1, OffStts: -1, OffStsz: -1, OffStco: -1, OffElst: -1}
			out.Tracks = append(out.Tracks, cur)
			return h.Expand()
		case mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(),
			mp4.BoxTypeEdts(), mp4.BoxTypeDinf(), mp4.BoxTypeStsd(),
			mp4.BoxTypeAvc1(), mp4.BoxTypeHvc1(), mp4.BoxTypeHev1(), mp4.BoxTypeMp4a():
			return h.Expand()
		case mp4.BoxTypeMvhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			out.MovieTimescale = int64(box.(*mp4.Mvhd).Timescale)
			return nil, nil
		}
		if !inTrak {
			return nil, nil
		}
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, fmt.Errorf("track %d box %s: %w", cur.Number, h.BoxInfo.Type, err)
		}
		off := int64(h.BoxInfo.Offset)
		switch b := box.(type) {
		case *mp4.Tkhd:
			cur.TrackID = b.TrackID
		case *mp4.Mdhd:
			cur.Timescale = int64(b.Timescale)
			cur.MediaDuration = int64(b.GetDuration())
		case *mp4.Hdlr:
			cur.Handler = string(b.HandlerType[:])
		case *mp4.Stts:
			cur.Stts, cur.OffStts = b.Entries, off
		case *mp4.Stsz:
			cur.OffStsz = off
			if b.SampleSize != 0 {
				cur.SampleSizes = make([]uint32, b.SampleCount)
				for i := range cur.SampleSizes {
					cur.SampleSizes[i] = b.SampleSize
				}
			} else {
				cur.SampleSizes = b.EntrySize
			}
		case *mp4.Stss:
			cur.SyncSamples = b.SampleNumber
		case *mp4.Stco:
			cur.ChunkOffsets, cur.OffStco = b.ChunkOffset, off
		case *mp4.Stsc:
			cur.Stsc = b.Entries
		case *mp4.Elst:
			cur.OffElst = off
			for i := range b.Entries {
				mt := b.GetMediaTime(i)
				cur.Edits = append(cur.Edits, clock.Edit{
					MediaTime: mt, Duration: int64(b.GetSegmentDuration(i)), Empty: mt < 0,
				})
			}
		case *mp4.VisualSampleEntry:
			cur.Width, cur.Height = int(b.Width), int(b.Height)
		case *mp4.AudioSampleEntry:
			cur.Channels, cur.SampleRate = int(b.ChannelCount), int(b.GetSampleRateInt())
		case *mp4.Colr:
			if string(b.ColourType[:]) == "nclx" {
				cur.Color = &ColorInfo{Primaries: b.ColourPrimaries, Transfer: b.TransferCharacteristics,
					Matrix: b.MatrixCoefficients, FullRange: b.FullRangeFlag}
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return out, nil
}
