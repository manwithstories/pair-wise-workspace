package mux

import (
	"io"

	"github.com/abema/go-mp4"

	"mediapack/internal/codec"
	"mediapack/internal/fault"
)

// Manifest is the provenance record emitted beside each MP4.
type Manifest struct {
	Codec       string `json:"codec"`
	TrackID     uint32 `json:"track_id"`
	Samples     int    `json:"samples"`
	Keyframes   int    `json:"keyframes"`
	Timescale   uint32 `json:"timescale"`
	Duration    uint64 `json:"duration"`
	PayloadOnly bool   `json:"payload_only"`
}

// Write produces an independently parseable MP4 from one parsed stream.
// w must be an io.WriteSeeker (the caller provides an fsynced file).
func Write(w io.WriteSeeker, units []codec.AccessUnit, meta *codec.StreamMeta) (*Manifest, error) {
	samples := buildSamples(units)
	if len(samples) == 0 {
		return nil, fault.New(fault.KindCorruptInput, "mux", "no samples to mux")
	}
	mw := mp4.NewWriter(w)
	const timescale = uint32(90000)
	duration := uint64(units[len(units)-1].PTS-units[0].PTS) + codecFrameDelta

	// ftyp
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return nil, fault.Wrap(err, "mux")
	}
	if _, err := mp4.Marshal(mw, &mp4.Ftyp{
		MajorBrand:     brand("iso4"),
		MinorVersion:   0,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: brand("iso4")},
			{CompatibleBrand: brand("mp41")},
		},
	}, mp4.Context{}); err != nil {
		return nil, fault.Wrap(err, "mux")
	}
	if _, err := mw.EndBox(); err != nil {
		return nil, fault.Wrap(err, "mux")
	}

	// mdat is written first so chunk offsets are known when moov is emitted.
	// Chunks group up to chunkBytes samples each, keeping the offset table
	// small while preserving every sample's size and timing.
	mdatStart, chunkOffsets, err := writeMdat(w, mw, samples, chunkBytes)
	if err != nil {
		return nil, err
	}

	if err := writeMoov(mw, meta, samples, timescale, duration, chunkOffsets); err != nil {
		return nil, err
	}
	_ = mdatStart

	keys := 0
	for _, s := range samples {
		if s.Key {
			keys++
		}
	}
	return &Manifest{
		Codec: meta.Codec.String(), TrackID: meta.TrackID, Samples: len(samples),
		Keyframes: keys, Timescale: timescale, Duration: duration, PayloadOnly: true,
	}, nil
}

const codecFrameDelta = uint64(3000)

// chunkBytes bounds how many sample bytes accumulate in one chunk.
const chunkBytes = 32 * 1024

// writeMdat writes the media box and returns the mdat start plus chunk offsets.
func writeMdat(w io.WriteSeeker, mw *mp4.Writer, samples []Sample,
	chunkBudget int) (uint64, []uint64, error) {
	start, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()})
	if err != nil {
		return 0, nil, fault.Wrap(err, "mux")
	}
	var offsets []uint64
	chunkSize := 0
	for _, s := range samples {
		if chunkSize == 0 {
			pos, err := w.Seek(0, io.SeekCurrent)
			if err != nil {
				return 0, nil, fault.Wrap(err, "mux")
			}
			offsets = append(offsets, uint64(pos))
		}
		if _, err := w.Write(s.Data); err != nil {
			return 0, nil, fault.Wrap(err, "mux")
		}
		chunkSize += len(s.Data)
		if chunkSize >= chunkBudget {
			chunkSize = 0
		}
	}
	if _, err := mw.EndBox(); err != nil {
		return 0, nil, fault.Wrap(err, "mux")
	}
	return start.Offset, offsets, nil
}

func brand(s string) [4]byte {
	var b [4]byte
	copy(b[:], s)
	return b
}

func writeMoov(mw *mp4.Writer, meta *codec.StreamMeta, samples []Sample,
	timescale uint32, duration uint64, chunkOffsets []uint64) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return fault.Wrap(err, "mux")
	}
	if err := box(mw, mp4.BoxTypeMvhd(), &mp4.Mvhd{
		Timescale: timescale, DurationV0: uint32(duration),
		Rate: 0x00010000, Volume: 0x0100,
		Matrix: identityMatrix(), NextTrackID: 2,
	}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return fault.Wrap(err, "mux")
	}
	if err := box(mw, mp4.BoxTypeTkhd(), &mp4.Tkhd{
		TrackID: meta.TrackID + 1, DurationV0: uint32(duration),
		Width: 320 << 16, Height: 240 << 16,
		Volume: 0, Matrix: identityMatrix(),
	}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return fault.Wrap(err, "mux")
	}
	if err := box(mw, mp4.BoxTypeMdhd(), &mp4.Mdhd{
		Timescale: timescale, DurationV0: uint32(duration),
		Language: lang("und"),
	}); err != nil {
		return err
	}
	if err := box(mw, mp4.BoxTypeHdlr(), &mp4.Hdlr{
		HandlerType: brand("vide"), Name: "mediapack video",
	}); err != nil {
		return err
	}
	if err := writeMinf(mw, meta, samples, timescale, chunkOffsets); err != nil {
		return err
	}
	// writeMinf closes minf itself; here mdia and trak remain open.
	if err := endAll(mw, 2); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // moov
		return fault.Wrap(err, "mux")
	}
	return nil
}

func marshal(mw *mp4.Writer, box mp4.IBox) error {
	if _, err := mp4.Marshal(mw, box, mp4.Context{}); err != nil {
		return fault.Wrap(err, "mux")
	}
	return nil
}

func endAll(mw *mp4.Writer, n int) error {
	for i := 0; i < n; i++ {
		if _, err := mw.EndBox(); err != nil {
			return fault.Wrap(err, "mux")
		}
	}
	return nil
}

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

func lang(s string) [3]byte {
	var l [3]byte
	for i := 0; i < 3 && i < len(s); i++ {
		l[i] = s[i] - 0x60 // ISO-639-2/T packed form
	}
	return l
}
