package remux

import (
	"github.com/abema/go-mp4"

	"mediqc/internal/media"
)

// Fragment is one track-scoped CMAF media fragment.
type Fragment struct {
	Sequence       uint32
	TrackID        int
	BaseDecodeTime uint64
	Samples        []media.Sample
}

// BuildFMP4 returns an init segment followed by deterministic moof/mdat fragments.
func BuildFMP4(tracks []media.Track, fragments []Fragment) ([]byte, *media.Error) {
	ws := &byteWriteSeeker{}
	w := mp4.NewWriter(ws)
	duration := estimateDuration(tracks, fragments)
	if err := writeInit(w, tracks, duration); err != nil {
		return nil, &media.Error{Code: media.CodeInternal, Component: "remux.fmp4", Message: err.Error()}
	}

	for _, frag := range fragments {
		if len(frag.Samples) == 0 {
			continue
		}
		moofSize, merr := measureMoof(frag)
		if merr != nil {
			return nil, merr
		}
		dataOffset := int32(moofSize + 8) // moof box then mdat box header
		if err := writeMoof(w, frag, dataOffset); err != nil {
			return nil, &media.Error{Code: media.CodeInternal, Component: "remux.fmp4", Message: err.Error()}
		}
		payload := make([]byte, 0)
		for _, s := range frag.Samples {
			payload = append(payload, s.Data...)
		}
		if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
			return nil, &media.Error{Code: media.CodeInternal, Component: "remux.fmp4", Message: err.Error()}
		}
		if _, err := w.Write(payload); err != nil {
			return nil, &media.Error{Code: media.CodeInternal, Component: "remux.fmp4", Message: err.Error()}
		}
		if _, err := w.EndBox(); err != nil {
			return nil, &media.Error{Code: media.CodeInternal, Component: "remux.fmp4", Message: err.Error()}
		}
	}
	return ws.Bytes(), nil
}

func measureMoof(frag Fragment) (int, *media.Error) {
	tmp := &byteWriteSeeker{}
	tw := mp4.NewWriter(tmp)
	if err := writeMoof(tw, frag, 0); err != nil {
		return 0, &media.Error{Code: media.CodeInternal, Component: "remux.fmp4", Message: err.Error()}
	}
	return len(tmp.Bytes()), nil
}

func writeMoof(w *mp4.Writer, frag Fragment, dataOffset int32) error {
	return container(w, boxType("moof"), func(w *mp4.Writer) error {
		if err := leaf(w, &mp4.Mfhd{SequenceNumber: frag.Sequence}); err != nil {
			return err
		}
		return container(w, boxType("traf"), func(w *mp4.Writer) error {
			tfhd := &mp4.Tfhd{TrackID: uint32(frag.TrackID)}
			tfhd.Flags = [3]byte{0x02, 0x00, 0x00} // default-base-is-moof
			if err := leaf(w, tfhd); err != nil {
				return err
			}
			tfdt := &mp4.Tfdt{BaseMediaDecodeTimeV1: frag.BaseDecodeTime}
			tfdt.Version = 1
			if err := leaf(w, tfdt); err != nil {
				return err
			}
			entries := make([]mp4.TrunEntry, 0, len(frag.Samples))
			for _, s := range frag.Samples {
				dur := s.Duration
				if dur == 0 {
					dur = 3000
				}
				entries = append(entries, mp4.TrunEntry{
					SampleDuration: dur,
					SampleSize:     uint32(len(s.Data)),
				})
			}
			trun := &mp4.Trun{
				SampleCount: uint32(len(entries)),
				DataOffset:  dataOffset,
				Entries:     entries,
			}
			// data-offset=0x000001, durations=0x000100, sizes=0x000200.
			trun.Flags = [3]byte{0x00, 0x03, 0x01}
			return leaf(w, trun)
		})
	})
}

func estimateDuration(tracks []media.Track, fragments []Fragment) uint32 {
	var maxEnd int64
	for _, f := range fragments {
		base := int64(f.BaseDecodeTime)
		for _, s := range f.Samples {
			d := int64(s.Duration)
			if d == 0 {
				d = 3000
			}
			base += d
		}
		if base > maxEnd {
			maxEnd = base
		}
	}
	if maxEnd <= 0 {
		for _, tr := range tracks {
			if tr.Timescale > 0 {
				return tr.Timescale
			}
		}
		return 90000
	}
	if maxEnd > 1<<32-1 {
		return 1<<32 - 1
	}
	return uint32(maxEnd)
}
