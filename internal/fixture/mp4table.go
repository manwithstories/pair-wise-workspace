package fixture

import mp4 "github.com/abema/go-mp4"

var identityMatrix = [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}

func writeFtyp(bw *boxWriter) error {
	if err := bw.start(mp4.BoxTypeFtyp()); err != nil {
		return err
	}
	err := bw.box(&mp4.Ftyp{
		MajorBrand:       [4]byte{'i', 's', 'o', 'm'},
		MinorVersion:     0x200,
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}},
	})
	if err != nil {
		return err
	}
	return bw.end()
}

func writeMoov(bw *boxWriter, spec MP4Spec, tracks []*builtTrack) error {
	if err := bw.start(mp4.BoxTypeMoov()); err != nil {
		return err
	}
	// Movie timescale follows the first track; duration is the longest track.
	var movieDuration uint64
	movieTS := uint32(1000)
	if len(tracks) > 0 {
		movieTS = tracks[0].spec.Timescale
		for _, t := range tracks {
			d := uint64(t.spec.SampleCount) * uint64(t.spec.SampleDelta)
			if nd := d * uint64(movieTS) / uint64(t.spec.Timescale); nd > movieDuration {
				movieDuration = nd
			}
		}
	}
	if err := bw.box(&mp4.Mvhd{
		Timescale:  movieTS,
		DurationV0: uint32(movieDuration),
		Rate:       0x00010000,
		Volume:     0x0100,
		Matrix:     identityMatrix,
		NextTrackID: uint32(len(tracks) + 1),
	}); err != nil {
		return err
	}
	for _, t := range tracks {
		if err := writeTrak(bw, t, movieTS); err != nil {
			return err
		}
	}
	return bw.end()
}

func writeTrak(bw *boxWriter, t *builtTrack, movieTS uint32) error {
	s := t.spec
	duration := uint64(s.SampleCount) * uint64(s.SampleDelta)
	if err := bw.start(mp4.BoxTypeTrak()); err != nil {
		return err
	}
	if err := bw.box(&mp4.Tkhd{
		TrackID:    t.trackID,
		DurationV0: uint32(duration * uint64(movieTS) / uint64(s.Timescale)),
		Volume:     tkhdVolume(s.Kind),
		Matrix:     identityMatrix,
		Width:      uint32(s.Width) << 16,
		Height:     uint32(s.Height) << 16,
	}); err != nil {
		return err
	}
	if s.IncludeEditList || s.BadClipEdit {
		if err := writeEdts(bw, t, duration, movieTS); err != nil {
			return err
		}
	}
	if err := bw.start(mp4.BoxTypeMdia()); err != nil {
		return err
	}
	if err := bw.box(&mp4.Mdhd{
		Timescale:  s.Timescale,
		DurationV0: uint32(duration),
		Language:   [3]byte{'u', 'n', 'd'},
	}); err != nil {
		return err
	}
	if err := bw.box(&mp4.Hdlr{
		HandlerType: handlerType(s.Kind),
		Name:        "avscan-fixture-" + s.Kind,
	}); err != nil {
		return err
	}
	if err := writeMinf(bw, t); err != nil {
		return err
	}
	if err := bw.end(); err != nil { // mdia
		return err
	}
	return bw.end() // trak
}

func writeEdts(bw *boxWriter, t *builtTrack, duration uint64, movieTS uint32) error {
	s := t.spec
	if err := bw.start(mp4.BoxTypeEdts()); err != nil {
		return err
	}
	if err := bw.start(mp4.BoxTypeElst()); err != nil {
		return err
	}
	mediaTime := int64(0)
	segDur := duration * uint64(movieTS) / uint64(s.Timescale)
	if s.BadClipEdit {
		// Deliberately points beyond the track's media duration.
		mediaTime = int64(duration) + int64(s.SampleDelta)*10
	}
	err := bw.box(&mp4.Elst{
		EntryCount: 1,
		Entries: []mp4.ElstEntry{{
			SegmentDurationV0: uint32(segDur),
			MediaTimeV0:       int32(mediaTime),
			MediaRateInteger:  1,
		}},
	})
	if err != nil {
		return err
	}
	if err := bw.end(); err != nil { // elst
		return err
	}
	return bw.end() // edts
}

func writeMinf(bw *boxWriter, t *builtTrack) error {
	s := t.spec
	if err := bw.start(mp4.BoxTypeMinf()); err != nil {
		return err
	}
	switch s.Kind {
	case "video":
		if err := bw.box(&mp4.Vmhd{}); err != nil {
			return err
		}
	case "audio":
		if err := bw.box(&mp4.Smhd{}); err != nil {
			return err
		}
	default:
		if err := bw.box(&nmhdBox{}); err != nil {
			return err
		}
	}
	if err := writeDinf(bw); err != nil {
		return err
	}
	if err := writeStbl(bw, t); err != nil {
		return err
	}
	return bw.end() // minf
}

func writeDinf(bw *boxWriter) error {
	if err := bw.start(mp4.BoxTypeDinf()); err != nil {
		return err
	}
	if err := bw.start(mp4.BoxTypeDref()); err != nil {
		return err
	}
	if err := bw.payload(&mp4.Dref{EntryCount: 1}); err != nil {
		return err
	}
	if err := bw.start(mp4.BoxTypeUrl()); err != nil {
		return err
	}
	if err := bw.payload(&mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}); err != nil {
		return err
	}
	if err := bw.end(); err != nil { // url
		return err
	}
	if err := bw.end(); err != nil { // dref
		return err
	}
	return bw.end() // dinf
}

func tkhdVolume(kind string) int16 {
	if kind == "audio" {
		return 0x0100
	}
	return 0
}

func handlerType(kind string) [4]byte {
	switch kind {
	case "video":
		return [4]byte{'v', 'i', 'd', 'e'}
	case "audio":
		return [4]byte{'s', 'o', 'u', 'n'}
	default:
		return [4]byte{'m', 'e', 't', 'a'}
	}
}
