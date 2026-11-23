package parse

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"archivecheck/internal/fault"
)

// Track describes one track declared by an initialization segment.
type Track struct {
	ID              uint32
	Timescale       uint32
	Handler         string
	DefaultDuration uint32
	DefaultSize     uint32
	DefaultFlags    uint32
	HasTrexDefaults bool
}

// Init is the parsed state of an initialization segment.
type Init struct {
	Segment string
	Brand   string
	Tracks  map[uint32]*Track
	Order   []uint32
}

// TrackByID returns the declared track or an offset-bearing fault.
func (in *Init) TrackByID(id uint32) (*Track, error) {
	if tr, ok := in.Tracks[id]; ok {
		return tr, nil
	}
	return nil, fault.New(fault.KindMissingBox, in.Segment, "trex", 0,
		fmt.Sprintf("track %d is not declared by the initialization segment", id))
}

// Sample is a concrete sample range within a media segment.
type Sample struct {
	TrackID           uint32
	Duration          uint32
	Size              uint32
	Flags             uint32
	CompositionOffset int32
	Offset            int64
}

// Sync reports whether the sample is a random-access point.
func (s Sample) Sync() bool { return s.Flags&0x00010000 == 0 }

// Traf is one track fragment with its decode timeline anchor.
type Traf struct {
	TrackID        uint32
	BaseDecodeTime uint64
	HasTfdt        bool
	TfdtVersion    uint8
	Samples        []Sample
}

// Fragment is a parsed media segment: one moof plus its mdat.
type Fragment struct {
	Segment     string
	Sequence    uint32
	HasMfhd     bool
	MfhdOffset  int64
	MoofOffset  int64
	MoofSize    uint64
	MdatOffset  int64
	MdatSize    uint64
	MdatPayload int64
	MdatEnd     int64
	Trafs       []Traf
}

func unmarshalChild(payload []byte, base int64, segment, typ string, dst mp4.IBox) error {
	br := bytes.NewReader(payload)
	if _, err := mp4.Unmarshal(br, uint64(len(payload)), dst, mp4.Context{}); err != nil {
		return fault.Wrap(err, fault.KindMalformed, segment, typ, base,
			fmt.Sprintf("cannot decode %s payload of %d bytes", typ, len(payload)))
	}
	return nil
}

// ParseInit reads ftyp and moov from an initialization segment file.
func ParseInit(f *os.File, segment string) (*Init, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fault.Wrap(err, fault.KindShortRead, segment, "", 0, "stat failed")
	}
	return ParseInitReader(f, info.Size(), segment)
}

// ParseInitReader reads ftyp and moov from an arbitrary reader.
func ParseInitReader(r io.ReadSeeker, size int64, segment string) (*Init, error) {
	init := &Init{Segment: segment, Tracks: map[uint32]*Track{}}
	type pending struct {
		id        uint32
		timescale uint32
		handler   string
	}
	// keyed by track order within moov
	var tracks []pending
	err := WalkReader(r, size, segment, func(h Header, r io.ReadSeeker) error {
		switch h.Type {
		case "ftyp":
			p, err := ReadPayload(r, h, segment)
			if err != nil {
				return err
			}
			var ftyp mp4.Ftyp
			if err := unmarshalChild(p, h.PayloadOffset(), segment, "ftyp", &ftyp); err != nil {
				return err
			}
			init.Brand = PrintableType(ftyp.MajorBrand[:])
		case "moov":
			p, err := ReadPayload(r, h, segment)
			if err != nil {
				return err
			}
			moovBase := h.PayloadOffset()
			return WalkPayload(p, moovBase, segment, func(ch Header, cp []byte) error {
				switch ch.Type {
				case "trak":
					tr := pending{}
					err := WalkPayload(cp, ch.PayloadOffset(), segment, func(th Header, tp []byte) error {
						switch th.Type {
						case "tkhd":
							var tkhd mp4.Tkhd
							if err := unmarshalChild(tp, th.PayloadOffset(), segment, "tkhd", &tkhd); err != nil {
								return err
							}
							tr.id = tkhd.TrackID
						case "mdia":
							return WalkPayload(tp, th.PayloadOffset(), segment, func(mh Header, mp []byte) error {
								switch mh.Type {
								case "mdhd":
									var mdhd mp4.Mdhd
									if err := unmarshalChild(mp, mh.PayloadOffset(), segment, "mdhd", &mdhd); err != nil {
										return err
									}
									tr.timescale = mdhd.Timescale
								case "hdlr":
									var hdlr mp4.Hdlr
									if err := unmarshalChild(mp, mh.PayloadOffset(), segment, "hdlr", &hdlr); err != nil {
										return err
									}
									tr.handler = PrintableType(hdlr.HandlerType[:])
								}
								return nil
							})
						}
						return nil
					})
					if err != nil {
						return err
					}
					tracks = append(tracks, tr)
				case "mvex":
					return WalkPayload(cp, ch.PayloadOffset(), segment, func(xh Header, xp []byte) error {
						if xh.Type != "trex" {
							return nil
						}
						var trex mp4.Trex
						if err := unmarshalChild(xp, xh.PayloadOffset(), segment, "trex", &trex); err != nil {
							return err
						}
						tr, ok := init.Tracks[trex.TrackID]
						if !ok {
							tr = &Track{ID: trex.TrackID}
							init.Tracks[trex.TrackID] = tr
						}
						tr.DefaultDuration = trex.DefaultSampleDuration
						tr.DefaultSize = trex.DefaultSampleSize
						tr.DefaultFlags = trex.DefaultSampleFlags
						tr.HasTrexDefaults = true
						return nil
					})
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, p := range tracks {
		tr, ok := init.Tracks[p.id]
		if !ok {
			tr = &Track{ID: p.id}
			init.Tracks[p.id] = tr
		}
		tr.Timescale = p.timescale
		tr.Handler = p.handler
	}
	for _, tr := range init.Tracks {
		if tr.Timescale == 0 {
			return nil, fault.New(fault.KindInvalidSize, segment, "mdhd", 0,
				fmt.Sprintf("track %d has zero timescale", tr.ID))
		}
	}
	init.Order = make([]uint32, 0, len(init.Tracks))
	seen := map[uint32]bool{}
	for _, p := range tracks {
		if !seen[p.id] {
			seen[p.id] = true
			init.Order = append(init.Order, p.id)
		}
	}
	return init, nil
}

type parsedTraf struct {
	tfhd     *mp4.Tfhd
	tfdt     *mp4.Tfdt
	tfdtBase int64
	truns    []parsedTrun
}

type parsedTrun struct {
	trun  *mp4.Trun
	base  int64
	flags uint32
}

// ParseFragment reads one media segment file and validates every sample
// range against its following mdat.
func ParseFragment(f *os.File, segment string, init *Init) (*Fragment, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fault.Wrap(err, fault.KindShortRead, segment, "", 0, "stat failed")
	}
	return ParseFragmentReader(f, info.Size(), segment, init)
}

// ParseFragmentReader reads a single moof+mdat segment from a reader.
func ParseFragmentReader(r io.ReadSeeker, size int64, segment string, init *Init) (*Fragment, error) {
	var moofPayload []byte
	var moof, mdat Header
	seenMoof := false
	seenMdat := false
	err := WalkReader(r, size, segment, func(h Header, rr io.ReadSeeker) error {
		switch h.Type {
		case "moof":
			if seenMoof {
				return fault.New(fault.KindMalformed, segment, "moof", h.Offset, "multiple moof boxes in one media segment")
			}
			seenMoof = true
			moof = h
			p, err := ReadPayload(rr, h, segment)
			if err != nil {
				return err
			}
			moofPayload = p
		case "mdat":
			if !seenMoof || moofPayload == nil {
				return fault.New(fault.KindMalformed, segment, "mdat", h.Offset, "mdat precedes moof")
			}
			if seenMdat {
				return fault.New(fault.KindMalformed, segment, "mdat", h.Offset, "multiple mdat boxes in one media segment")
			}
			seenMdat = true
			mdat = h
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !seenMoof {
		return nil, fault.New(fault.KindMissingBox, segment, "moof", 0, "media segment has no moof")
	}
	if !seenMdat {
		return nil, fault.New(fault.KindMissingBox, segment, "mdat", moof.Offset, "moof has no following mdat")
	}
	return ParseFragmentBoxes(moofPayload, moof, mdat, segment, init)
}

// ParseFragmentBoxes validates one moof payload against its mdat header.
func ParseFragmentBoxes(moofPayload []byte, moof, mdat Header, segment string, init *Init) (*Fragment, error) {
	if init == nil {
		return nil, fault.New(fault.KindMissingBox, segment, "moov", 0, "no initialization segment")
	}
	frag := &Fragment{
		Segment: segment, MoofOffset: moof.Offset, MoofSize: moof.Size,
		MdatOffset: mdat.Offset, MdatSize: mdat.Size,
		MdatPayload: mdat.PayloadOffset(), MdatEnd: mdat.End(),
	}
	if err := parseMoof(frag, moofPayload, moof.PayloadOffset(), segment, init); err != nil {
		return nil, err
	}
	return frag, nil
}

func parseMoof(frag *Fragment, data []byte, base int64, segment string, init *Init) error {
	var trafs []*parsedTraf
	baseOffset := frag.MoofOffset
	explicitBase := false
	mfhdCount := 0
	err := WalkPayload(data, base, segment, func(h Header, payload []byte) error {
		switch h.Type {
		case "mfhd":
			mfhdCount++
			if mfhdCount > 1 {
				return fault.New(fault.KindMalformed, segment, "mfhd", h.Offset, "multiple mfhd boxes in one moof")
			}
			var mfhd mp4.Mfhd
			if err := unmarshalChild(payload, h.PayloadOffset(), segment, "mfhd", &mfhd); err != nil {
				return err
			}
			frag.HasMfhd = true
			frag.Sequence = mfhd.SequenceNumber
			frag.MfhdOffset = h.Offset
		case "traf":
			pt := &parsedTraf{}
			if err := WalkPayload(payload, h.PayloadOffset(), segment, func(ch Header, cp []byte) error {
				switch ch.Type {
				case "tfhd":
					var tfhd mp4.Tfhd
					if err := unmarshalChild(cp, ch.PayloadOffset(), segment, "tfhd", &tfhd); err != nil {
						return err
					}
					pt.tfhd = &tfhd
					if tfhd.CheckFlag(mp4.TfhdBaseDataOffsetPresent) {
						baseOffset = int64(tfhd.BaseDataOffset)
						explicitBase = true
					}
				case "tfdt":
					var tfdt mp4.Tfdt
					if err := unmarshalChild(cp, ch.PayloadOffset(), segment, "tfdt", &tfdt); err != nil {
						return err
					}
					pt.tfdt = &tfdt
					pt.tfdtBase = ch.Offset
				case "trun":
					var trun mp4.Trun
					if err := unmarshalChild(cp, ch.PayloadOffset(), segment, "trun", &trun); err != nil {
						return err
					}
					pt.truns = append(pt.truns, parsedTrun{trun: &trun, base: ch.Offset, flags: trun.GetFlags()})
				}
				return nil
			}); err != nil {
				return err
			}
			trafs = append(trafs, pt)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(trafs) == 0 {
		return fault.New(fault.KindMissingBox, segment, "traf", frag.MoofOffset, "moof contains no traf")
	}
	if !explicitBase {
		baseOffset = frag.MoofOffset
	}

	for _, pt := range trafs {
		if pt.tfhd == nil {
			return fault.New(fault.KindMissingBox, segment, "tfhd", frag.MoofOffset, "traf has no tfhd")
		}
		track, err := init.TrackByID(pt.tfhd.TrackID)
		if err != nil {
			if fe, ok := fault.As(err); ok {
				fe.Offset = frag.MoofOffset
			}
			return err
		}
		if pt.tfdt == nil {
			return fault.New(fault.KindMissingBox, segment, "tfdt", frag.MoofOffset,
				fmt.Sprintf("traf for track %d has no tfdt", pt.tfhd.TrackID))
		}
		if len(pt.truns) == 0 {
			return fault.New(fault.KindMissingBox, segment, "trun", frag.MoofOffset,
				fmt.Sprintf("traf for track %d has no trun", pt.tfhd.TrackID))
		}
		traf := Traf{
			TrackID:        pt.tfhd.TrackID,
			HasTfdt:        true,
			TfdtVersion:    pt.tfdt.GetVersion(),
			BaseDecodeTime: pt.tfdt.GetBaseMediaDecodeTime(),
		}
		cursor := baseOffset
		for _, ptn := range pt.truns {
			tr := ptn.trun
			if ptn.flags&0x000001 != 0 {
				cursor = baseOffset + int64(tr.DataOffset)
			}
			if tr.SampleCount > 0 && len(tr.Entries) < int(tr.SampleCount) {
				return fault.New(fault.KindShortRead, segment, "trun", ptn.base,
					fmt.Sprintf("trun declares %d samples but decodes %d entries", tr.SampleCount, len(tr.Entries)))
			}
			for i := 0; i < int(tr.SampleCount); i++ {
				e := tr.Entries[i]
				duration := resolveDuration(e, ptn.flags, pt.tfhd, track)
				size := resolveSize(e, ptn.flags, pt.tfhd, track)
				flags := resolveFlags(i, e, ptn.flags, tr, pt.tfhd, track)
				if duration == 0 {
					return fault.New(fault.KindMalformed, segment, "trun", ptn.base,
						fmt.Sprintf("sample %d of track %d has zero duration", i, track.ID))
				}
				if size == 0 {
					return fault.New(fault.KindMalformed, segment, "trun", ptn.base,
						fmt.Sprintf("sample %d of track %d has zero size", i, track.ID))
				}
				if cursor < frag.MdatPayload || cursor > frag.MdatEnd || int64(size) > frag.MdatEnd-cursor {
					return fault.New(fault.KindOutOfBounds, segment, "trun", ptn.base,
						fmt.Sprintf("sample %d of track %d range [%d,%d) escapes mdat payload [%d,%d)",
							i, track.ID, cursor, cursor+int64(size), frag.MdatPayload, frag.MdatEnd))
				}
				traf.Samples = append(traf.Samples, Sample{
					TrackID:           track.ID,
					Duration:          duration,
					Size:              size,
					Flags:             flags,
					CompositionOffset: int32(tr.GetSampleCompositionTimeOffset(i)),
					Offset:            cursor,
				})
				cursor += int64(size)
			}
		}
		frag.Trafs = append(frag.Trafs, traf)
	}
	return nil
}

func resolveDuration(e mp4.TrunEntry, flags uint32, tfhd *mp4.Tfhd, track *Track) uint32 {
	if flags&0x000100 != 0 {
		return e.SampleDuration
	}
	if tfhd.CheckFlag(mp4.TfhdDefaultSampleDurationPresent) {
		return tfhd.DefaultSampleDuration
	}
	return track.DefaultDuration
}

func resolveSize(e mp4.TrunEntry, flags uint32, tfhd *mp4.Tfhd, track *Track) uint32 {
	if flags&0x000200 != 0 {
		return e.SampleSize
	}
	if tfhd.CheckFlag(mp4.TfhdDefaultSampleSizePresent) {
		return tfhd.DefaultSampleSize
	}
	return track.DefaultSize
}

func resolveFlags(i int, e mp4.TrunEntry, flags uint32, tr *mp4.Trun, tfhd *mp4.Tfhd, track *Track) uint32 {
	if i == 0 && flags&0x000004 != 0 {
		return tr.FirstSampleFlags
	}
	if flags&0x000400 != 0 {
		return e.SampleFlags
	}
	if tfhd.CheckFlag(mp4.TfhdDefaultSampleFlagsPresent) {
		return tfhd.DefaultSampleFlags
	}
	return track.DefaultFlags
}
