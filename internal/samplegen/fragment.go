package samplegen

import "strconv"

// FragSample is one trun sample description.
type FragSample struct {
	Duration uint32
	Size     uint32
	NonSync  bool
}

// Fragment is one tfdt/trun/mdat unit.
type Fragment struct {
	BaseTime   uint64
	Samples    []FragSample
	TfdtMarker string
	TrunMarker string
}

// FragTrack groups one track's fragments.
type FragTrack struct {
	ID        uint32
	Handler   string
	Timescale uint32
	Fragments []Fragment
}

func buildFragmented(quickTime bool, tracks []FragTrack) *normalBuilt {
	ftyp := ftypBox(quickTime)
	var traks []*Builder
	var trex []*Builder
	for _, t := range tracks {
		spec := TrackSpec{ID: t.ID, Handler: t.Handler, Timescale: t.Timescale}
		stbl := New("stbl", nil,
			stsdBox(t.Handler),
			Full("stts", 0, 0, u32(0)),
			Full("stsc", 0, 0, u32(0)),
			Full("stsz", 0, 0, appendBytes(u32(0), u32(0))),
			Full("stco", 0, 0, u32(0)),
		)
		header := Full("smhd", 0, 0, appendBytes(u16(0), u16(0)))
		if t.Handler != "soun" {
			header = Full("vmhd", 0, 1, make([]byte, 8))
		}
		minf := New("minf", nil, header, dinfBox(), stbl)
		mdia := New("mdia", nil, mdhdBox(spec), hdlrBox(t.Handler), minf)
		traks = append(traks, New("trak", nil, tkhdBox(t.ID, t.Handler), mdia))
		trexBody := appendBytes(u32(t.ID), u32(1), u32(1), u32(0), u32(0))
		trex = append(trex, Full("trex", 0, 0, trexBody))
	}
	moov := New("moov", nil, append([]*Builder{mvhdBox()}, append(traks, New("mvex", nil, trex...))...)...)

	var data []byte
	marks := map[string]int{}
	data = append(data, ftyp.serialize(marks, 0)...)
	moovStart := len(data)
	data = append(data, moov.serialize(marks, moovStart)...)

	for _, track := range tracks {
		for fi, frag := range track.Fragments {
			tfdtPayload := u32(uint32(frag.BaseTime)) // v0 is sufficient for fixtures
			tfdt := Full("tfdt", 0, 0, tfdtPayload)
			if frag.TfdtMarker != "" {
				tfdt.Mark(frag.TfdtMarker)
			}
			tfhd := Full("tfhd", 0, 0, u32(track.ID))
			trunBody := u32(uint32(len(frag.Samples)))
			trunBody = append(trunBody, u32(0)...) // data_offset placeholder
			for _, s := range frag.Samples {
				flags := uint32(0)
				if s.NonSync {
					flags = 0x00010000
				}
				trunBody = appendBytes(trunBody, u32(s.Duration), u32(s.Size), u32(flags))
			}
			trun := Full("trun", 0, 0x000701, trunBody)
			trunMarker := frag.TrunMarker
			if trunMarker == "" {
				trunMarker = trunMarkerName(track.ID, fi)
			}
			trun.Mark(trunMarker)
			mfhd := Full("mfhd", 0, 0, u32(uint32(fi+1)))
			traf := New("traf", nil, tfhd, tfdt, trun)
			moof := New("moof", nil, mfhd, traf)

			moofStart := len(data)
			moofBytes := moof.serialize(marks, moofStart)
			data = append(data, moofBytes...)
			dataOffset := uint32(len(moofBytes) + 8)
			patchU32(data, marks[trunMarker]+16, dataOffset)

			var mdatPayload []byte
			for i, s := range frag.Samples {
				for j := uint32(0); j < s.Size; j++ {
					mdatPayload = append(mdatPayload, byte(track.ID*17+uint32(i)*31+j*7))
				}
			}
			mdat := New("mdat", mdatPayload)
			data = append(data, mdat.serialize(marks, len(data))...)
		}
	}
	return &normalBuilt{Data: data, Marks: marks}
}

func trunMarkerName(trackID uint32, fragment int) string {
	return "trun-" + strconv.Itoa(int(trackID)) + "-" + strconv.Itoa(fragment)
}

func cleanFragTracks() []FragTrack {
	mk := func(id uint32, handler string, ts uint32) FragTrack {
		samples := []FragSample{
			{Duration: 1001, Size: 16, NonSync: false},
			{Duration: 1001, Size: 16, NonSync: true},
			{Duration: 1001, Size: 16, NonSync: false},
			{Duration: 1001, Size: 16, NonSync: true},
		}
		if handler == "soun" {
			samples = []FragSample{
				{Duration: 1024, Size: 8, NonSync: false},
				{Duration: 1024, Size: 8, NonSync: true},
				{Duration: 1024, Size: 8, NonSync: false},
				{Duration: 1024, Size: 8, NonSync: true},
			}
		}
		return FragTrack{ID: id, Handler: handler, Timescale: ts, Fragments: []Fragment{{BaseTime: 0, Samples: samples}}}
	}
	return []FragTrack{mk(1, "vide", 30000), mk(2, "soun", 48000)}
}
