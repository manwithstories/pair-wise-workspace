package samplegen

import "strconv"

// NormalOptions controls ordinary sample-table fixture layout and mutations.
type NormalOptions struct {
	QuickTime   bool
	UnknownBox  bool
	MDATFirst   bool
	Seed        int64
	TruncatedAt string // marker of an stbl box to truncate after its header+4
	ExtraSTBL   map[uint32][]*Builder
}

type normalBuilt struct {
	Data  []byte
	Marks map[string]int
}

func buildNormal(specs []TrackSpec, opts NormalOptions) *normalBuilt {
	var top []*Builder
	_ = top

	ftyp := ftypBox(opts.QuickTime)
	traks := make([]*Builder, 0, len(specs))
	for _, spec := range specs {
		marker := stcoMarker(spec.ID)
		var extra []*Builder
		if opts.ExtraSTBL != nil {
			extra = opts.ExtraSTBL[spec.ID]
		}
		traks = append(traks, trakBox(spec, marker, extra...))
	}
	moov := New("moov", nil, append([]*Builder{mvhdBox()}, traks...)...)
	if opts.UnknownBox {
		// A valid unknown box is added at top level after the regular boxes.
		// It is appended below after mdat/moov ordering as needed.
	}
	unknown := New("zzzz", []byte("scan"))

	sampleData := make([]byte, 0)
	seedByte := byte(opts.Seed)
	for si, spec := range specs {
		for i := 0; i < spec.SampleCount*int(spec.SampleSize); i++ {
			sampleData = append(sampleData, byte(17)+seedByte+byte(si*31+i*7))
		}
	}
	mdat := New("mdat", sampleData).Mark("mdat")

	var data []byte
	marks := map[string]int{}
	appendAt := func(b *Builder, start int) []byte {
		bd := b.serialize(marks, start)
		return bd
	}

	ftypBytes := ftyp.serialize(marks, 0)
	data = append(data, ftypBytes...)
	var mdatPayloadStart int
	if opts.MDATFirst {
		mdatStart := len(data)
		mdatBytes := appendAt(mdat, mdatStart)
		data = append(data, mdatBytes...)
		mdatPayloadStart = mdatStart + 8
		moovStart := len(data)
		moovBytes := appendAt(moov, moovStart)
		data = append(data, moovBytes...)
		if opts.UnknownBox {
			uStart := len(data)
			data = append(data, unknown.serialize(marks, uStart)...)
		}
	} else {
		moovStart := len(data)
		moovBytes := appendAt(moov, moovStart)
		data = append(data, moovBytes...)
		mdatStart := len(data)
		mdatBytes := appendAt(mdat, mdatStart)
		data = append(data, mdatBytes...)
		mdatPayloadStart = mdatStart + 8
		if opts.UnknownBox {
			uStart := len(data)
			data = append(data, unknown.serialize(marks, uStart)...)
		}
	}

	cursor := mdatPayloadStart
	for _, spec := range specs {
		boxStart := marks[stcoMarker(spec.ID)]
		firstValueOffset := boxStart + 16
		for i := 0; i < spec.SampleCount; i++ {
			off := firstValueOffset + i*4
			patchU32(data, off, uint32(cursor))
			cursor += int(spec.SampleSize)
		}
	}

	if opts.TruncatedAt != "" {
		if start, ok := marks[opts.TruncatedAt]; ok {
			data = data[:start+12] // full header + four payload bytes
		}
	}
	return &normalBuilt{Data: data, Marks: marks}
}

func stcoMarker(trackID uint32) string {
	return "stco-" + strconv.Itoa(int(trackID))
}

func trakBox(spec TrackSpec, stcoMarkerName string, extraSTBL ...*Builder) *Builder {
	stblChildren := []*Builder{
		stsdBox(spec.Handler),
		sttsBox(spec),
		stscBox(),
		stszBox(spec),
		stcoBox(spec, stcoMarkerName),
	}
	if len(spec.Keyframes) > 0 {
		stblChildren = append(stblChildren, stssBox(spec.Keyframes))
	}
	stblChildren = append(stblChildren, extraSTBL...)
	stbl := New("stbl", nil, stblChildren...)

	var mediaHeader *Builder
	if spec.Handler == "soun" {
		mediaHeader = Full("smhd", 0, 0, appendBytes(u16(0), u16(0)))
	} else {
		mediaHeader = Full("vmhd", 0, 1, make([]byte, 8))
	}
	minf := New("minf", nil, mediaHeader, dinfBox(), stbl)
	mdia := New("mdia", nil, mdhdBox(spec), hdlrBox(spec.Handler), minf)

	trakChildren := []*Builder{tkhdBox(spec.ID, spec.Handler)}
	if spec.IncludeEdit {
		trakChildren = append(trakChildren, New("edts", nil, elstBox(spec)))
	}
	trakChildren = append(trakChildren, mdia)
	return New("trak", nil, trakChildren...)
}

// StandardSpecs returns the deterministic two-track pattern used by most
// fixtures. Timescales differ intentionally.
func StandardSpecs(videoTS, audioTS uint32) []TrackSpec {
	return []TrackSpec{
		{ID: 1, Handler: "vide", Timescale: videoTS, SampleCount: 6, Delta: 1001, SampleSize: 16, Keyframes: []int{1, 4}},
		{ID: 2, Handler: "soun", Timescale: audioTS, SampleCount: 6, Delta: 1024, SampleSize: 8, Keyframes: []int{1, 3, 5}},
	}
}
