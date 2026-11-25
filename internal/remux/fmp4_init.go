package remux

import (
	"github.com/abema/go-mp4"

	"mediqc/internal/media"
)

func writeInit(w *mp4.Writer, tracks []media.Track, duration uint32) error {
	ftyp := &mp4.Ftyp{
		MajorBrand:   [4]byte{'C', 'M', 'F', 'C'},
		MinorVersion: 0x00000001,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', '5'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', '6'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}
	if err := leaf(w, ftyp); err != nil {
		return err
	}

	matrix := [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	return container(w, boxType("moov"), func(w *mp4.Writer) error {
		if err := leaf(w, &mp4.Mvhd{
			Timescale: 90000, DurationV0: duration, Rate: 0x00010000,
			Volume: 0x0100, Matrix: matrix, NextTrackID: uint32(len(tracks) + 1),
		}); err != nil {
			return err
		}
		for _, tr := range tracks {
			if err := writeTrack(w, tr, duration, matrix); err != nil {
				return err
			}
		}
		return container(w, boxType("mvex"), func(w *mp4.Writer) error {
			for _, tr := range tracks {
				if err := leaf(w, &mp4.Trex{
					TrackID: uint32(tr.ID), DefaultSampleDescriptionIndex: 1,
				}); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func writeTrack(w *mp4.Writer, tr media.Track, duration uint32, matrix [9]int32) error {
	handler := [4]byte{'v', 'i', 'd', 'e'}
	name := "MediaQC Video"
	volume := int16(0)
	width, height := uint32(320<<16), uint32(240<<16)
	if tr.Kind == media.TrackAudio {
		handler = [4]byte{'s', 'o', 'u', 'n'}
		name = "MediaQC Audio"
		volume = 0x0100
		width, height = 0, 0
	}
	return container(w, boxType("trak"), func(w *mp4.Writer) error {
		if err := leaf(w, &mp4.Tkhd{
			FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 3}},
			TrackID: uint32(tr.ID), DurationV0: duration,
			Volume: volume, Matrix: matrix, Width: width, Height: height,
		}); err != nil {
			return err
		}
		return container(w, boxType("mdia"), func(w *mp4.Writer) error {
			if err := leaf(w, &mp4.Mdhd{
				Timescale: 90000, DurationV0: duration, Language: [3]byte{'u', 'n', 'd'},
			}); err != nil {
				return err
			}
			if err := leaf(w, &mp4.Hdlr{HandlerType: handler, Name: name}); err != nil {
				return err
			}
			return container(w, boxType("minf"), func(w *mp4.Writer) error {
				if tr.Kind == media.TrackVideo {
					if err := leaf(w, &mp4.Vmhd{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}); err != nil {
						return err
					}
				} else if err := leaf(w, &mp4.Smhd{}); err != nil {
					return err
				}
				return container(w, boxType("stbl"), func(w *mp4.Writer) error {
					if err := leaf(w, &mp4.Stsd{EntryCount: 0}); err != nil {
						return err
					}
					if err := leaf(w, &mp4.Stts{}); err != nil {
						return err
					}
					if err := leaf(w, &mp4.Stsc{}); err != nil {
						return err
					}
					if err := leaf(w, &mp4.Stsz{}); err != nil {
						return err
					}
					return leaf(w, &mp4.Stco{})
				})
			})
		})
	})
}
