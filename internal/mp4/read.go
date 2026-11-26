package mp4

import (
	"encoding/json"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"mediad/internal/media"
)

// MaxSamplesPerTrack bounds defensive allocations. Sixty seconds at 60 fps has
// only 3600 video samples; this leaves broad headroom while rejecting hostile
// uint32 entry counts on a 2 GiB input.
const MaxSamplesPerTrack = 2_000_000

// ReadPath opens a local MP4/MOV and reads its normalized timetable.
func ReadPath(path string, salvage bool) (*media.Project, []*Box, []*media.Fault, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, media.NewFault(media.CodeInvalidInput, "open input: %v", err).Fatal()
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, nil, media.NewFault(media.CodeInvalidInput, "stat input: %v", err).Fatal()
	}
	if st.IsDir() {
		return nil, nil, nil, media.NewFault(media.CodeInvalidInput, "input is a directory").Fatal()
	}
	return ReadProject(f, st.Size(), salvage)
}

// ReadProject scans box boundaries and decodes all track timetables.
func ReadProject(r io.ReadSeeker, fileSize int64, salvage bool) (*media.Project, []*Box, []*media.Fault, error) {
	var boxes []*Box
	var scanFaults []*media.Fault
	var err error
	if salvage {
		boxes, scanFaults, err = ScanSalvage(r, fileSize)
	} else {
		boxes, err = Scan(r, fileSize)
	}
	if err != nil {
		return nil, boxes, scanFaults, err
	}
	moov := findTopBox(boxes, "moov")
	if moov == nil {
		return nil, boxes, scanFaults, media.NewFault(media.CodeInvalidInput, "missing moov box").Fatal()
	}
	trakBoxes := moov.All("trak")
	if len(trakBoxes) == 0 {
		return nil, boxes, scanFaults, media.NewFault(media.CodeInvalidInput, "moov contains no trak boxes").Fatal()
	}
	if len(trakBoxes) > MaxTracks {
		return nil, boxes, scanFaults, media.NewFault(media.CodeTooManyTracks, "tracks=%d limit=%d", len(trakBoxes), MaxTracks).Fatal()
	}

	p := &media.Project{MovieTimescale: 1000, SourceFormat: "mp4"}
	p.Faults = append(p.Faults, scanFaults...)
	for i, tb := range trakBoxes {
		track, faults, err := readTrack(r, tb, fileSize, boxes)
		if err != nil {
			return nil, boxes, p.Faults, err
		}
		if track == nil {
			continue
		}
		if track.ID == 0 {
			track.ID = uint32(i + 1)
		}
		track.Faults = append(track.Faults, faults...)
		p.Faults = append(p.Faults, faults...)
		p.Tracks = append(p.Tracks, track)
	}
	if len(p.Tracks) == 0 {
		return nil, boxes, p.Faults, media.NewFault(media.CodeInvalidInput, "no readable track timetables").Fatal()
	}
	return p, boxes, p.Faults, nil
}

func findTopBox(boxes []*Box, typ string) *Box {
	for _, b := range boxes {
		if b.Type == typ {
			return b
		}
	}
	return nil
}

func readTrack(r io.ReadSeeker, tb *Box, fileSize int64, top []*Box) (*media.Track, []*media.Fault, error) {
	var faults []*media.Fault
	t := &media.Track{}

	if tkhdBox := tb.Find("tkhd"); tkhdBox != nil {
		var tkhd gomp4.Tkhd
		if err := unmarshalTypedBox(r, tkhdBox, &tkhd); err != nil {
			return nil, faults, err
		}
		t.ID = tkhd.TrackID
		t.Width = uint16(tkhd.Width >> 16)
		t.Height = uint16(tkhd.Height >> 16)
	}
	mdhdBox := tb.Find("mdia", "mdhd")
	if mdhdBox == nil {
		return nil, faults, media.NewFault(media.CodeInvalidInput, "track %d missing mdhd", t.ID).WithTrack(t.ID).Fatal()
	}
	var mdhd gomp4.Mdhd
	if err := unmarshalTypedBox(r, mdhdBox, &mdhd); err != nil {
		return nil, faults, err
	}
	t.Timescale = mdhd.Timescale
	if t.Timescale == 0 {
		f := media.NewFault(media.CodeInvalidInput, "mdhd timescale is zero").WithTrack(t.ID).WithBox("mdhd").Fatal()
		return nil, faults, f
	}
	if hdlrBox := tb.Find("mdia", "hdlr"); hdlrBox != nil {
		var hdlr gomp4.Hdlr
		if err := unmarshalTypedBox(r, hdlrBox, &hdlr); err == nil {
			t.Kind = media.TrackKind(hdlr.HandlerType[:])
		}
	}
	if t.Kind == "" {
		t.Kind = media.KindData
	}

	stbl := tb.Find("mdia", "minf", "stbl")
	if stbl == nil {
		return nil, faults, media.NewFault(media.CodeInvalidInput, "track %d missing stbl", t.ID).WithTrack(t.ID).Fatal()
	}
	samples, tableFaults, err := readSampleTables(r, t.ID, stbl)
	if err != nil {
		return nil, faults, err
	}
	faults = append(faults, tableFaults...)
	t.Samples = samples

	if elstBox := tb.Find("edts", "elst"); elstBox != nil {
		edits, err := readEditList(r, t.ID, elstBox)
		if err != nil {
			return nil, faults, err
		}
		t.Edits = edits
	}
	if audt := tb.Find("audt"); audt != nil {
		if payload, err := readBoxPayload(r, audt); err == nil {
			var a media.AudioMetadata
			if err := json.Unmarshal(payload.b, &a); err != nil {
				faults = append(faults, media.NewFault(media.CodeAudioLayout, "invalid audt metadata: %v", err).WithTrack(t.ID).WithBox("audt"))
			} else {
				t.Audio = &a
			}
		}
	}
	if vcol := tb.Find("vcol"); vcol != nil {
		if payload, err := readBoxPayload(r, vcol); err == nil {
			var c media.ColorMetadata
			if err := json.Unmarshal(payload.b, &c); err != nil {
				faults = append(faults, media.NewFault(media.CodeColorConflict, "invalid vcol metadata: %v", err).WithTrack(t.ID).WithBox("vcol"))
			} else {
				t.Color = &c
			}
		}
	}

	offsetFaults := validateOffsets(t, stbl, fileSize, top)
	faults = append(faults, offsetFaults...)
	timelineFaults := detectTimelineFaults(t)
	faults = append(faults, timelineFaults...)
	return t, faults, nil
}

func readEditList(r io.ReadSeeker, trackID uint32, b *Box) ([]media.EditEntry, error) {
	var elst gomp4.Elst
	if err := unmarshalTypedBox(r, b, &elst); err != nil {
		return nil, media.NewFault(media.CodeEditListConflict, "decode elst: %v", err).WithTrack(trackID).WithBox("elst").Fatal()
	}
	out := make([]media.EditEntry, 0, elst.EntryCount)
	for i := range elst.Entries {
		e := &elst.Entries[i]
		if elst.Version == 1 {
			out = append(out, media.EditEntry{SegmentDuration: e.SegmentDurationV1, MediaTime: e.MediaTimeV1, MediaRate: e.MediaRateInteger})
		} else {
			out = append(out, media.EditEntry{SegmentDuration: uint64(e.SegmentDurationV0), MediaTime: int64(e.MediaTimeV0), MediaRate: e.MediaRateInteger})
		}
	}
	return out, nil
}
