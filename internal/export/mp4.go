package export

import (
	"io"
	"os"
	"path/filepath"

	mp4 "github.com/abema/go-mp4"
)

// TrackTiming carries one track's clock information into the container.
type TrackTiming struct {
	TrackID       uint32 `json:"track_id"`
	Timescale     uint32 `json:"timescale"`
	DurationTicks uint64 `json:"duration_ticks"`
	Frames        uint64 `json:"frames"`
}

// WriteMP4 writes a local MP4 container carrying per-track timing
// (mvhd/mdhd/tkhd) with the frame sequence embedded as mdat payload.
// All timestamps are fixed values so the bytes are deterministic.
// The file is written via temp+rename like every other artifact.
func WriteMP4(path string, tracks []TrackTiming, mdat io.Reader) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mp4-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	defer func() {
		if cerr := tmp.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	w := mp4.NewWriter(tmp)
	if _, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	_, err = mp4.Marshal(w, &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 1,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}, mp4.Context{})
	if err != nil {
		return err
	}
	if _, err = w.EndBox(); err != nil {
		return err
	}

	// moov
	if _, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	var maxDur uint64
	for _, t := range tracks {
		if t.DurationTicks > maxDur {
			maxDur = t.DurationTicks
		}
	}
	if err = writeBox(w, &mp4.Mvhd{
		Timescale:   48000,
		DurationV0:  uint32(maxDur),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      unityMatrix,
		NextTrackID: uint32(len(tracks)) + 1,
	}); err != nil {
		return err
	}
	for _, t := range tracks {
		if err = writeTrak(w, t); err != nil {
			return err
		}
	}
	if _, err = w.EndBox(); err != nil { // moov
		return err
	}

	// mdat
	if _, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return err
	}
	if _, err = io.Copy(w, mdat); err != nil {
		return err
	}
	if _, err = w.EndBox(); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

var unityMatrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}

func writeBox(w *mp4.Writer, box mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func writeTrak(w *mp4.Writer, t TrackTiming) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Tkhd{
		TrackID:    t.TrackID + 1,
		DurationV0: uint32(t.DurationTicks),
		Volume:     0x0100,
		Matrix:     unityMatrix,
	}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Mdhd{
		Timescale:  t.Timescale,
		DurationV0: uint32(t.DurationTicks),
		Language:   [3]byte{'u', 'n', 'd'},
	}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Hdlr{
		HandlerType: [4]byte{'s', 'o', 'u', 'n'},
		Name:        "medpipe track",
	}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil { // mdia
		return err
	}
	_, err := w.EndBox() // trak
	return err
}
