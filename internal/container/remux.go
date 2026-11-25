package container

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"archivectl/internal/timeline"
)

// BuildFTYP returns a compatible ISO-BMFF file type box.
func BuildFTYP() []byte {
	p := make([]byte, 16)
	copy(p[0:4], "isom")
	binary.BigEndian.PutUint32(p[4:8], 512)
	copy(p[8:12], "isom")
	copy(p[12:16], "mp41")
	return BuildBox("ftyp", p)
}

func hdlrBox(kind string) []byte {
	handler := KindFourCC(kind)
	p := make([]byte, 25)
	binary.BigEndian.PutUint32(p[0:4], 0) // version/flags are supplied by BuildFullBox
	copy(p[4:8], handler)
	copy(p[8:12], "appl")
	binary.BigEndian.PutUint32(p[12:16], 0)
	binary.BigEndian.PutUint32(p[16:20], 0)
	copy(p[20:], "ArchiveHandler")
	return BuildFullBox("hdlr", 0, 0, p)
}

func trakBox(t Track) []byte {
	return BuildBox("trak", append(hdlrBox(t.Kind), EncodeTrackIndex(t)...))
}

func statusBox(a *Asset) []byte {
	p := make([]byte, 17)
	if a.Partial {
		p[0] = 1
	}
	binary.BigEndian.PutUint32(p[1:5], uint32(a.GapPackets))
	binary.BigEndian.PutUint32(p[5:9], uint32(a.GapRanges))
	binary.BigEndian.PutUint64(p[9:17], uint64(a.SourceBytes))
	return BuildBox("psta", p)
}

// BuildMP4 deterministically serializes an asset using the bounded archive
// index and mdat layout. Sample bytes are copied verbatim.
func BuildMP4(a *Asset) ([]byte, error) {
	if a == nil {
		return nil, timeline.New(timeline.CodeInvalidArgument, "build mp4", "nil asset")
	}
	for _, t := range a.Tracks {
		if err := timeline.CheckDTSMonotonic(t.ID, dtsValues(t.Samples)); err != nil {
			return nil, err
		}
		for i, s := range t.Samples {
			if s.Size != len(s.Data) {
				return nil, &timeline.Error{Code: timeline.CodeIntegrity, Op: "build mp4", TrackID: t.ID,
					Detail: fmt.Sprintf("sample %d size field %d != bytes %d", i, s.Size, len(s.Data)), Offset: -1}
			}
			if s.Hash != "" && s.Hash != HashBytes(s.Data) {
				return nil, &timeline.Error{Code: timeline.CodeIntegrity, Op: "build mp4", TrackID: t.ID,
					Detail: fmt.Sprintf("sample %d hash conflict", i), Offset: s.FileOffset}
			}
		}
	}

	ftyp := BuildFTYP()
	mvhd := BuildFullBox("mvhd", 0, 0, make([]byte, 96))
	eventBoxes := rawEventBoxes(a)
	placeholderChildren := append([]byte{}, mvhd...)
	placeholderChildren = append(placeholderChildren, eventBoxes...)
	for _, t := range a.Tracks {
		placeholderChildren = append(placeholderChildren, trakBox(t)...)
	}
	placeholderChildren = append(placeholderChildren, statusBox(a)...)
	placeholderMoov := BuildBox("moov", placeholderChildren)
	dataStart := len(ftyp) + len(placeholderMoov) + boxHeaderSize32

	// Copy tracks while assigning absolute mdat offsets. Encoded integer width
	// does not change, so moov size and dataStart remain stable.
	tracks := make([]Track, len(a.Tracks))
	var mdat bytes.Buffer
	for ti, t := range a.Tracks {
		t.Samples = append([]Sample(nil), t.Samples...)
		for i := range t.Samples {
			s := &t.Samples[i]
			ensureSampleHash(s)
			offset := int64(dataStart) + int64(mdat.Len())
			end, overflow := addOverflow(offset, int64(len(s.Data)))
			if overflow || end > MaxBoxSize {
				return nil, timeline.New(timeline.CodeOverflow, "build mp4", "mdat offset exceeds bounded file")
			}
			s.FileOffset = offset
			s.Size = len(s.Data)
			if _, err := mdat.Write(s.Data); err != nil {
				return nil, err
			}
		}
		tracks[ti] = t
	}

	children := append([]byte{}, mvhd...)
	children = append(children, eventBoxes...)
	for _, t := range tracks {
		children = append(children, trakBox(t)...)
	}
	children = append(children, statusBox(a)...)
	moov := BuildBox("moov", children)
	if len(ftyp)+len(moov)+boxHeaderSize32 != dataStart {
		return nil, timeline.New(timeline.CodeIntegrity, "build mp4", "layout offset changed")
	}
	mdatBox := BuildBox("mdat", mdat.Bytes())
	if len(ftyp)+len(moov)+len(mdatBox) > MaxBoxSize {
		return nil, timeline.New(timeline.CodeOverflow, "build mp4", "output exceeds bounded maximum")
	}
	var out bytes.Buffer
	out.Write(ftyp)
	out.Write(moov)
	out.Write(mdatBox)
	return out.Bytes(), nil
}

func rawEventBoxes(a *Asset) []byte {
	var b []byte
	for _, e := range a.Events {
		if e.Kind == EventEMSG && len(e.RawBox) > 0 {
			b = append(b, e.RawBox...)
		}
	}
	return b
}

// RemuxResult contains output identity and fidelity counters.
type RemuxResult struct {
	Bytes               []byte
	OutputHash          string
	SampleCount         int
	PayloadBytes        int64
	PayloadHashMismatch int
}

// RemuxAsset serializes the demuxed asset without touching sample codecs.
func RemuxAsset(a *Asset) (RemuxResult, error) {
	b, err := BuildMP4(a)
	if err != nil {
		return RemuxResult{}, err
	}
	r := RemuxResult{Bytes: b, OutputHash: HashBytes(b)}
	for _, t := range a.Tracks {
		r.SampleCount += len(t.Samples)
		for _, s := range t.Samples {
			r.PayloadBytes += int64(len(s.Data))
			if s.Hash != HashBytes(s.Data) {
				r.PayloadHashMismatch++
			}
		}
	}
	return r, nil
}

// BuildInitSegment writes a movie initialization box sequence.
func BuildInitSegment(tracks []Track) []byte {
	a := &Asset{Tracks: tracks}
	b, err := BuildMP4(a)
	if err != nil {
		panic(err) // all callers construct bounded valid tracks
	}
	return b
}

// BuildFragment builds one moof/mdat fragment with the archive sample index.
func BuildFragment(sequence uint32, track Track, emsgEvents ...Event) ([]byte, error) {
	if len(track.Samples) == 0 {
		return nil, timeline.New(timeline.CodeInvalidArgument, "build fragment", "fragment has no samples")
	}
	base := track.Samples[0].DTS
	mfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdPayload, sequence)
	mfhd := BuildBox("mfhd", mfhdPayload)

	tfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(tfhdPayload, track.ID)
	tfhd := BuildFullBox("tfhd", 0, 0x020000, tfhdPayload) // default-base-is-moof-like flag ignored by local parser

	tfdtPayload := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtPayload, uint64(base))
	tfdt := BuildFullBox("tfdt", 1, 0, tfdtPayload)

	stypPayload := make([]byte, 8)
	copy(stypPayload[0:4], "msdh")
	binary.BigEndian.PutUint32(stypPayload[4:8], 0)
	styp := BuildBox("styp", stypPayload)

	eventBoxes := rawEventBoxes(&Asset{Events: emsgEvents})
	index := EncodeTrackIndex(track)
	traf := BuildBox("traf", append(append(tfhd, tfdt...), index...))
	placeholderMoof := BuildBox("moof", append(append(mfhd, eventBoxes...), traf...))
	dataStart := len(styp) + len(placeholderMoof) + boxHeaderSize32

	var mdat bytes.Buffer
	for i := range track.Samples {
		s := &track.Samples[i]
		ensureSampleHash(s)
		s.FileOffset = int64(dataStart + mdat.Len())
		s.Size = len(s.Data)
		mdat.Write(s.Data)
	}
	index = EncodeTrackIndex(track)
	traf = BuildBox("traf", append(append(tfhd, tfdt...), index...))
	moof := BuildBox("moof", append(append(mfhd, eventBoxes...), traf...))
	if len(styp)+len(moof)+boxHeaderSize32 != dataStart {
		return nil, timeline.New(timeline.CodeIntegrity, "build fragment", "fragment layout changed")
	}
	return append(append(append([]byte{}, styp...), moof...), BuildBox("mdat", mdat.Bytes())...), nil
}
