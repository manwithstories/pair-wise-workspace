// Package pack writes and structurally validates fragmented MP4 output.
package pack

import (
	"bytes"
	"fmt"

	"github.com/abema/go-mp4"

	"mediad/internal/timeline"
)

// TimescaleNS is a nanosecond MP4 timescale, avoiding alignment rounding.
const TimescaleNS uint32 = 1_000_000_000

// Sample is one access unit in a media fragment.
type Sample struct {
	Data       []byte
	DurationNS int64
	Keyframe   bool
}

// Fragment describes one keyframe-aligned segment.
type Fragment struct {
	SequenceNumber uint32
	StartNS        int64
	Samples        []Sample
}

func brand(s string) [4]byte {
	var b [4]byte
	copy(b[:], s)
	return b
}

func compatible(brands ...string) []mp4.CompatibleBrandElem {
	out := make([]mp4.CompatibleBrandElem, 0, len(brands))
	for _, b := range brands {
		out = append(out, mp4.CompatibleBrandElem{CompatibleBrand: brand(b)})
	}
	return out
}

func box(w *mp4.Writer, typ mp4.BoxType, fn func() error) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: typ}); err != nil {
		return err
	}
	if fn != nil {
		if err := fn(); err != nil {
			return err
		}
	}
	_, err := w.EndBox()
	return err
}

func leaf(w *mp4.Writer, b mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: b.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, b, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

// BuildInit creates a minimal parseable fMP4 init segment.
func BuildInit() ([]byte, error) {
	sb := &seekBuffer{}
	w := mp4.NewWriter(sb)
	ftyp := &mp4.Ftyp{
		MajorBrand:       brand("iso5"),
		MinorVersion:     512,
		CompatibleBrands: compatible("iso5", "iso6", "dash", "msdh"),
	}
	if err := leaf(w, ftyp); err != nil {
		return nil, classify(err)
	}

	err := box(w, mp4.BoxTypeMoov(), func() error {
		mvhd := &mp4.Mvhd{Timescale: TimescaleNS, Rate: 0x10000, Volume: 0x0100, NextTrackID: 1}
		mvhd.Matrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
		if err := leaf(w, mvhd); err != nil {
			return err
		}
		return box(w, mp4.BoxTypeTrak(), func() error {
			tkhd := &mp4.Tkhd{TrackID: 1, Width: 320 << 16, Height: 180 << 16}
			tkhd.SetFlags(0x000007) // enabled, in movie, in preview
			tkhd.Matrix = mvhd.Matrix
			if err := leaf(w, tkhd); err != nil {
				return err
			}
			return box(w, mp4.BoxTypeMdia(), func() error {
				mdhd := &mp4.Mdhd{Timescale: TimescaleNS}
				if err := leaf(w, mdhd); err != nil {
					return err
				}
				hdlr := &mp4.Hdlr{HandlerType: brand("vide"), Name: "mediad synthetic video"}
				if err := leaf(w, hdlr); err != nil {
					return err
				}
				return box(w, mp4.BoxTypeMinf(), func() error {
					vmhd := &mp4.Vmhd{}
					vmhd.SetFlags(1)
					if err := leaf(w, vmhd); err != nil {
						return err
					}
					return box(w, mp4.BoxTypeStbl(), func() error {
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
	})
	if err != nil {
		return nil, classify(err)
	}

	err = box(w, mp4.BoxTypeMvex(), func() error {
		return leaf(w, &mp4.Trex{
			TrackID:                       1,
			DefaultSampleDescriptionIndex: 1,
		})
	})
	if err != nil {
		return nil, classify(err)
	}
	out := append([]byte(nil), sb.Bytes()...)
	if err := Validate(out, "ftyp", "moov", "trak", "mdia", "stbl", "mvex", "trex"); err != nil {
		return nil, err
	}
	return out, nil
}

// BuildMedia creates an styp + moof/trun + mdat fragment and validates it.
func BuildMedia(f Fragment) ([]byte, error) {
	if len(f.Samples) == 0 {
		return nil, timeline.New(timeline.KindInternal, "pack.mp4", -1, -1, "media fragment has no samples")
	}
	var mdat []byte
	entries := make([]mp4.TrunEntry, 0, len(f.Samples))
	var totalDuration int64
	for _, s := range f.Samples {
		if s.DurationNS <= 0 || s.DurationNS > int64(^uint32(0)) {
			return nil, timeline.New(timeline.KindInternal, "pack.mp4", -1, -1,
				"invalid sample duration %d", s.DurationNS)
		}
		entries = append(entries, mp4.TrunEntry{
			SampleDuration: uint32(s.DurationNS),
			SampleSize:     uint32(len(s.Data)),
		})
		mdat = append(mdat, s.Data...)
		totalDuration += s.DurationNS
	}

	buildMoof := func(dataOffset int32) ([]byte, error) {
		sb := &seekBuffer{}
		w := mp4.NewWriter(sb)
		err := box(w, mp4.BoxTypeMoof(), func() error {
			if err := leaf(w, &mp4.Mfhd{SequenceNumber: f.SequenceNumber}); err != nil {
				return err
			}
			return box(w, mp4.BoxTypeTraf(), func() error {
				tfhd := &mp4.Tfhd{TrackID: 1}
				tfhd.SetFlags(mp4.TfhdDefaultBaseIsMoof)
				if err := leaf(w, tfhd); err != nil {
					return err
				}
				tfdt := &mp4.Tfdt{BaseMediaDecodeTimeV1: uint64(f.StartNS)}
				tfdt.SetVersion(1)
				if err := leaf(w, tfdt); err != nil {
					return err
				}
				trun := &mp4.Trun{
					SampleCount: uint32(len(entries)),
					DataOffset:  dataOffset,
					Entries:     entries,
				}
				trun.SetFlags(0x000301) // data-offset + sample duration + sample size
				return leaf(w, trun)
			})
		})
		return sb.Bytes(), err
	}

	// Data offset is relative to moof start when default-base-is-moof is set.
	// trun's fixed-width offset does not change moof size, so zero-pass size is final.
	moofZero, err := buildMoof(0)
	if err != nil {
		return nil, classify(err)
	}
	dataOffset := int32(len(moofZero) + 8) // + mdat header
	moof, err := buildMoof(dataOffset)
	if err != nil {
		return nil, classify(err)
	}

	sb := &seekBuffer{}
	w := mp4.NewWriter(sb)
	styp := &mp4.Styp{MajorBrand: brand("msdh"), MinorVersion: 0, CompatibleBrands: compatible("msdh", "dash")}
	if err := leaf(w, styp); err != nil {
		return nil, classify(err)
	}
	if _, err := w.Write(moof); err != nil {
		return nil, classify(err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return nil, classify(err)
	}
	if _, err := w.Write(mdat); err != nil {
		return nil, classify(err)
	}
	if _, err := w.EndBox(); err != nil {
		return nil, classify(err)
	}
	out := append([]byte(nil), sb.Bytes()...)
	if err := Validate(out, "styp", "moof", "mfhd", "traf", "tfhd", "tfdt", "trun", "mdat"); err != nil {
		return nil, err
	}
	return out, nil
}

func classify(err error) error {
	return timeline.Wrap(timeline.KindInternal, "pack.mp4", -1, -1, err)
}

// Validate parses box structure with go-mp4 and requires every wanted type.
func Validate(b []byte, wanted ...string) error {
	if len(b) < 8 {
		return timeline.New(timeline.KindInternal, "pack.validate", -1, -1, "fragment too short: %d bytes", len(b))
	}
	found := make(map[string]int)
	_, err := mp4.ReadBoxStructure(bytes.NewReader(b), func(h *mp4.ReadHandle) (any, error) {
		found[h.BoxInfo.Type.String()]++
		if h.ReadPayload != nil && (h.BoxInfo.Type == mp4.BoxTypeTrun() ||
			h.BoxInfo.Type == mp4.BoxTypeTfdt() || h.BoxInfo.Type == mp4.BoxTypeTfhd()) {
			if _, _, err := h.ReadPayload(); err != nil {
				return nil, fmt.Errorf("decode %s payload: %w", h.BoxInfo.Type, err)
			}
		}
		return nil, nil
	})
	if err != nil {
		return timeline.Wrap(timeline.KindInternal, "pack.validate", -1, -1, err)
	}
	for _, typ := range wanted {
		if found[typ] == 0 {
			return timeline.New(timeline.KindInternal, "pack.validate", -1, -1, "missing MP4 box %q (found=%v)", typ, found)
		}
	}
	return nil
}
