// Package mp4clip writes and reads minimal MP4 short clips with verifiable
// per-sample timestamps using github.com/abema/go-mp4. The clips carry one
// track (audio or video) whose stts/stsz/stco/stsc tables fully describe
// sample timing, letting the pipeline prove that the media timeline survives
// serialization without depending on any real encoder.
package mp4clip

import (
	"io"

	"github.com/abema/go-mp4"

	"mediakernel/internal/model"
)

// Sample is one clip sample: its duration in the track timescale and its raw
// (placeholder) payload bytes.
type Sample struct {
	Delta uint32
	Data  []byte
}

// Clip describes a single-track MP4 short clip.
type Clip struct {
	TrackID   uint32
	Timescale uint32
	IsVideo   bool
	Handler   string // "soun" or "vide"; derived when empty
	Samples   []Sample
	Width     uint16
	Height    uint16
}

// WriteClip serializes a structurally valid ftyp/mdat/moov MP4. The mdat box
// is written before moov so every chunk offset is known up front, avoiding
// placeholder patching. All allocations follow the caller-provided sample
// sizes; no length field in this format can trigger unbounded allocation.
func WriteClip(w io.WriteSeeker, clip Clip) error {
	if clip.Timescale == 0 || len(clip.Samples) == 0 {
		return &model.MediaError{
			Category: model.CatAlignment,
			Detail:   "mp4 clip requires a non-zero timescale and at least one sample",
			Decision: model.DecRejected,
		}
	}
	handler := clip.Handler
	if handler == "" {
		if clip.IsVideo {
			handler = "vide"
		} else {
			handler = "soun"
		}
	}
	mw := mp4.NewWriter(w)
	if err := writeFtyp(mw); err != nil {
		return err
	}
	chunkOffset, totalDuration, err := writeMdat(mw, clip)
	if err != nil {
		return err
	}
	return writeMoov(mw, clip, handler, chunkOffset, totalDuration)
}

func writeFtyp(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	ftyp := &mp4.Ftyp{
		MajorBrand: [4]byte{'i', 's', 'o', 'm'},
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '2'}},
		},
	}
	if _, err := mp4.Marshal(mw, ftyp, mp4.Context{}); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

// writeMdat returns the absolute chunk offset (single chunk holds every
// sample) and total duration in track timescale units.
func writeMdat(mw *mp4.Writer, clip Clip) (uint32, uint32, error) {
	pos, err := mw.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, 0, err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return 0, 0, err
	}
	chunkOffset := uint32(pos) + 8 // 8-byte mdat header
	var totalDuration uint32
	for _, s := range clip.Samples {
		if _, err := mw.Write(s.Data); err != nil {
			return 0, 0, err
		}
		totalDuration += s.Delta
	}
	if _, err := mw.EndBox(); err != nil {
		return 0, 0, err
	}
	return chunkOffset, totalDuration, nil
}
