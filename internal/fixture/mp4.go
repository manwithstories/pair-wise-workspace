// Package fixture deterministically generates the media samples that mediad's
// demo and tests operate on. Every byte comes from the supplied seed, so two
// runs with the same seed produce identical files, manifests and statistics.
package fixture

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"
)

// TrackSpec describes one synthetic track.
type TrackSpec struct {
	ID         uint32
	Kind       string // "video" or "audio"
	Codec      string // "avc1" or "mp4a"
	Timescale  uint32
	Frames     int
	FrameDelta uint32
	Width      uint16
	Height     uint16

	// Rotation is written into tkhd as a right-angle rotation.
	Rotation int
	// CropLeft/CropRight/CropTop/CropBottom are written as a clap box.
	CropLeft, CropRight, CropTop, CropBottom uint32
	// Color is written as a colr nclx box; nil leaves it out.
	Color *ColorSpec
	// ConflictColor, when set, is written as a second, disagreeing colr
	// box so the arbitration path can be exercised.
	ConflictColor *ColorSpec
	// SampleBytes is the payload size of each sample.
	SampleBytes int
	// GOPLength > 0 makes only every GOPLength-th sample a sync sample.
	GOPLength int
}

// ColorSpec is a container-level colour description.
type ColorSpec struct {
	Primaries, Transfer, Matrix uint16
	FullRange                   bool
}

// MP4Spec describes an MP4 fixture file.
type MP4Spec struct {
	Tracks []TrackSpec
	// EditMediaTime, when non-zero, is written as an elst media_time, so the
	// track's timeline starts that many ticks into the media.
	EditMediaTime int32
}

// WriteMP4 writes spec to path, returning the file size.
func buildMP4(out io.WriteSeeker, spec MP4Spec, seed int64) error {
	w := mp4.NewWriter(out)

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 512,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', '2'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}

	// Reserve mdat, then write moov after it. Offsets are computed from the
	// known payload layout, so the mdat header must be written first.
	mdatSize := int64(0)
	for _, t := range spec.Tracks {
		mdatSize += int64(t.Frames) * int64(t.SampleBytes)
	}
	mdatHeader := int64(8)
	if mdatSize+mdatHeader+8 > 1<<32 {
		// Keep the fixture well inside 32-bit offsets.
		return fmt.Errorf("fixture mdat too large: %d", mdatSize)
	}
	mdatStart, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(mdatSize+mdatHeader))
	copy(hdr[4:], "mdat")
	if _, err := out.Write(hdr); err != nil {
		return err
	}
	chunkOffsets := make([][]uint32, len(spec.Tracks))
	payloadStart := mdatStart + mdatHeader
	cur := payloadStart
	for ti, t := range spec.Tracks {
		chunkOffsets[ti] = make([]uint32, t.Frames)
		buf := make([]byte, t.SampleBytes)
		for i := 0; i < t.Frames; i++ {
			chunkOffsets[ti][i] = uint32(cur)
			buf = Payload(seed, t.ID, i, t.SampleBytes, buf)
			if _, err := out.Write(buf); err != nil {
				return err
			}
			cur += int64(t.SampleBytes)
		}
	}

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	totalDuration := uint32(0)
	for _, t := range spec.Tracks {
		if d := uint32(t.Frames) * t.FrameDelta; d > totalDuration {
			totalDuration = d
		}
	}
	if err := marshalBox(w, &mp4.Mvhd{
		Timescale:   1000,
		DurationV0:  totalDuration * 1000 / spec.Tracks[0].Timescale,
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      identityMatrix(),
		NextTrackID: uint32(len(spec.Tracks) + 1),
	}); err != nil {
		return err
	}
	for ti, t := range spec.Tracks {
		if err := buildTrak(w, t, chunkOffsets[ti], spec.EditMediaTime); err != nil {
			return err
		}
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	return nil
}
