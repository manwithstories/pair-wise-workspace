package fixture

import (
	"github.com/abema/go-mp4"
)

func buildTrak(w *mp4.Writer, t TrackSpec, offsets []uint32, editMediaTime int32) error {
	start := func(typ mp4.BoxType) error {
		_, err := w.StartBox(&mp4.BoxInfo{Type: typ})
		return err
	}
	end := func() error { _, err := w.EndBox(); return err }

	if err := start(mp4.BoxTypeTrak()); err != nil {
		return err
	}
	dur := uint32(t.Frames) * t.FrameDelta
	if editMediaTime > 0 {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeEdts()}); err != nil {
			return err
		}
		if err := marshalBox(w, &mp4.Elst{
			EntryCount: 1,
			Entries: []mp4.ElstEntry{{
				SegmentDurationV0: dur,
				MediaTimeV0:       editMediaTime,
				MediaRateInteger:  1,
			}},
		}); err != nil {
			return err
		}
		if _, err := w.EndBox(); err != nil {
			return err
		}
	}
	if err := marshalBox(w, &mp4.Tkhd{
		TrackID:    t.ID,
		DurationV0: dur,
		Matrix:     rotationMatrix(t.Rotation),
		Width:      uint32(t.Width) << 16,
		Height:     uint32(t.Height) << 16,
	}); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeMdia()); err != nil {
		return err
	}
	if err := marshalBox(w, &mp4.Mdhd{
		Timescale:  t.Timescale,
		DurationV0: dur,
		Language:   [3]byte{'e', 'n', 'g'},
	}); err != nil {
		return err
	}
	handler := [4]byte{'v', 'i', 'd', 'e'}
	if t.Kind == "audio" {
		handler = [4]byte{'s', 'o', 'u', 'n'}
	}
	if err := marshalBox(w, &mp4.Hdlr{HandlerType: handler, Name: "mediad"}); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeMinf()); err != nil {
		return err
	}
	if t.Kind == "audio" {
		if err := marshalBox(w, &mp4.Smhd{}); err != nil {
			return err
		}
	} else {
		if err := marshalBox(w, &mp4.Vmhd{}); err != nil {
			return err
		}
	}
	if err := start(mp4.BoxTypeDinf()); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeDref()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Dref{EntryCount: 1}, mp4.Context{}); err != nil {
		return err
	}
	// Dref's count is followed by child boxes; the marshal above writes the
	// count and the writer continues with the url child.
	if err := marshalBox(w, &mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}); err != nil {
		return err
	}
	if err := end(); err != nil { // dref
		return err
	}
	if err := end(); err != nil { // dinf
		return err
	}
	if err := start(mp4.BoxTypeStbl()); err != nil {
		return err
	}
	if err := buildStsd(w, t); err != nil {
		return err
	}
	if err := marshalBox(w, &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{
		{SampleCount: uint32(t.Frames), SampleDelta: t.FrameDelta},
	}}); err != nil {
		return err
	}
	if t.GOPLength > 0 {
		var sync []uint32
		for i := 1; i <= t.Frames; i += t.GOPLength {
			sync = append(sync, uint32(i))
		}
		if err := marshalBox(w, &mp4.Stss{EntryCount: uint32(len(sync)), SampleNumber: sync}); err != nil {
			return err
		}
	}
	if err := marshalBox(w, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{
		{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1},
	}}); err != nil {
		return err
	}
	sizes := make([]uint32, len(offsets))
	for i := range sizes {
		sizes[i] = uint32(t.SampleBytes)
	}
	if err := marshalBox(w, &mp4.Stsz{SampleCount: uint32(len(sizes)), EntrySize: sizes}); err != nil {
		return err
	}
	if err := marshalBox(w, &mp4.Stco{EntryCount: uint32(len(offsets)), ChunkOffset: offsets}); err != nil {
		return err
	}
	if err := end(); err != nil { // stbl
		return err
	}
	if err := end(); err != nil { // minf
		return err
	}
	if err := end(); err != nil { // mdia
		return err
	}
	return end() // trak
}
