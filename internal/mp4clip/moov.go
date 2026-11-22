package mp4clip

import (
	"github.com/abema/go-mp4"
)

// identityMatrix is the standard 0x00010000 identity MP4 transform.
var identityMatrix = [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}

func writeMoov(mw *mp4.Writer, clip Clip, handler string, chunkOffset, totalDuration uint32) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	// mvhd
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMvhd()}); err != nil {
		return err
	}
	mvhd := &mp4.Mvhd{
		Timescale:   clip.Timescale,
		DurationV0:  totalDuration,
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      identityMatrix,
		NextTrackID: clip.TrackID + 1,
	}
	if _, err := mp4.Marshal(mw, mvhd, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	// trak
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	if err := writeTkhd(mw, clip, totalDuration); err != nil {
		return err
	}
	if err := writeMdia(mw, clip, handler, chunkOffset, totalDuration); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // trak
		return err
	}
	if _, err := mw.EndBox(); err != nil { // moov
		return err
	}
	return nil
}

func writeTkhd(mw *mp4.Writer, clip Clip, totalDuration uint32) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTkhd()}); err != nil {
		return err
	}
	tkhd := &mp4.Tkhd{
		TrackID:    clip.TrackID,
		DurationV0: totalDuration,
		Volume:     volumeFor(clip.IsVideo),
		Matrix:     identityMatrix,
		Width:      uint32(clip.Width) << 16,
		Height:     uint32(clip.Height) << 16,
	}
	if _, err := mp4.Marshal(mw, tkhd, mp4.Context{}); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

func volumeFor(isVideo bool) int16 {
	if isVideo {
		return 0
	}
	return 0x0100
}

func writeMdia(mw *mp4.Writer, clip Clip, handler string, chunkOffset, totalDuration uint32) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	// mdhd
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdhd()}); err != nil {
		return err
	}
	mdhd := &mp4.Mdhd{
		Timescale:  clip.Timescale,
		DurationV0: totalDuration,
		Language:   [3]byte{'u', 'n', 'd'},
	}
	if _, err := mp4.Marshal(mw, mdhd, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	// hdlr
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeHdlr()}); err != nil {
		return err
	}
	hdlr := &mp4.Hdlr{
		HandlerType: str4(handler),
		Name:        "mediakernel-" + handler,
	}
	if _, err := mp4.Marshal(mw, hdlr, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	// minf > stbl
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}
	if err := writeStbl(mw, clip, chunkOffset); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // minf
		return err
	}
	if _, err := mw.EndBox(); err != nil { // mdia
		return err
	}
	return nil
}

func writeStbl(mw *mp4.Writer, clip Clip, chunkOffset uint32) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return err
	}
	n := uint32(len(clip.Samples))
	boxes := []struct {
		boxType mp4.BoxType
		payload mp4.IBox
	}{
		{mp4.BoxTypeStts(), buildStts(clip)},
		{mp4.BoxTypeStsc(), &mp4.Stsc{
			EntryCount: 1,
			Entries:    []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: n, SampleDescriptionIndex: 1}},
		}},
		{mp4.BoxTypeStsz(), buildStsz(clip, n)},
		{mp4.BoxTypeStco(), &mp4.Stco{
			EntryCount:  1,
			ChunkOffset: []uint32{chunkOffset},
		}},
	}
	for _, b := range boxes {
		if _, err := mw.StartBox(&mp4.BoxInfo{Type: b.boxType}); err != nil {
			return err
		}
		if _, err := mp4.Marshal(mw, b.payload, mp4.Context{}); err != nil {
			return err
		}
		if _, err := mw.EndBox(); err != nil {
			return err
		}
	}
	_, err := mw.EndBox() // stbl
	return err
}

func buildStts(clip Clip) *mp4.Stts {
	return &mp4.Stts{
		EntryCount: uint32(len(clip.Samples)),
		Entries:    sttsEntries(clip.Samples),
	}
}

func buildStsz(clip Clip, n uint32) *mp4.Stsz {
	sizes := make([]uint32, n)
	for i, s := range clip.Samples {
		sizes[i] = uint32(len(s.Data))
	}
	return &mp4.Stsz{SampleSize: 0, SampleCount: n, EntrySize: sizes}
}

func str4(s string) [4]byte {
	var out [4]byte
	copy(out[:], s)
	return out
}
