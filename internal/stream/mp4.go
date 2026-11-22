package stream

import (
	"bytes"
	"io"

	"github.com/abema/go-mp4"

	"mediascan/internal/media"
)

// boxPaths are the leaves the analyzer needs. Expansion is driven by these
// paths, so intermediate containers (moov, trak, mdia, minf, stbl) are walked
// automatically.
var boxPaths = []mp4.BoxPath{
	{mp4.BoxTypeFtyp()},
	// Container paths are listed so trak byte spans are observable; leaf
	// paths below additionally force expansion into each container.
	{mp4.BoxTypeMoov()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeMvhd()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeTkhd()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStts()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsc()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStco()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeCo64()},
	{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStss()},
	{mp4.BoxTypeMdat()},
}

// trakBoxes holds the leaf boxes belonging to one trak, located via
// byte-span containment.
type trakBoxes struct {
	tkhd      *mp4.BoxInfo
	mdhd      *mp4.BoxInfo
	hdlr      *mp4.BoxInfo
	stsd      *mp4.BoxInfo
	stts      *mp4.BoxInfo
	stsc      *mp4.BoxInfo
	stsz      *mp4.BoxInfo
	stco      *mp4.BoxInfo
	co64      *mp4.BoxInfo
	stss      *mp4.BoxInfo
	trakStart uint64
	trakEnd   uint64
}

// FileAnalysis is the result of inspecting one MP4 byte stream.
type FileAnalysis struct {
	Size       int64
	MajorBrand string
	TrackCount int
	// Tracks are keyed by TrackID in discovery order.
	Tracks []*Track
	Issues media.IssueSet
	// CorruptRanges locates damaged byte spans, one per damaged sample.
	CorruptRanges []media.Range
}

// AnalyzeMP4 parses the container structure and per-track sample tables.
// Structural corruption is reported as issues with byte locators rather than
// returned as a bare error; the returned error is reserved for input that
// cannot be interpreted at all (empty file, no moov).
func AnalyzeMP4(data []byte, limits Limits) (*FileAnalysis, error) {
	limits = limits.normalize()
	if int64(len(data)) > limits.MaxInputBytes {
		return nil, media.NewFatal(media.ClassMP4BadStructure, media.Locator{ByteLength: int64(len(data))},
			"MP4 input is %d bytes, limit is %d", len(data), limits.MaxTrackBytes)
	}
	if len(data) < 8 {
		return nil, media.NewFatal(media.ClassMP4Truncated, media.Locator{ByteLength: int64(len(data))},
			"input is %d bytes, too small to contain any box", len(data))
	}
	fa := &FileAnalysis{Size: int64(len(data))}
	r := bytes.NewReader(data)

	bis, err := mp4.ExtractBoxes(r, nil, boxPaths)
	if err != nil {
		// A truncated tail often manifests here. Record it and keep whatever
		// boxes were discovered before the failure.
		fa.Issues.Add(media.NewIssue(media.ClassMP4Truncated, media.Locator{ByteLength: int64(len(data))},
			"box structure scan failed: %v", err))
	}

	var mdatRanges []byteRange
	// Group boxes by track by walking in file order and tracking the current
	// trak via ancestor offset ordering. ExtractBoxes returns a flat post-order
	// list; we bucket by containment instead.
	var traks []*trakBoxes
	// Discover trak spans.
	trakInfos, _ := mp4.ExtractBoxes(r, nil, []mp4.BoxPath{{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()}})
	for _, ti := range trakInfos {
		traks = append(traks, &trakBoxes{trakStart: ti.Offset, trakEnd: ti.Offset + ti.Size})
	}
	findTrak := func(off uint64) *trakBoxes {
		for _, t := range traks {
			if off >= t.trakStart && off < t.trakEnd {
				return t
			}
		}
		return nil
	}
	var moovSeen bool
	for _, bi := range bis {
		switch bi.Type {
		case mp4.BoxTypeFtyp():
			ftyp := &mp4.Ftyp{}
			if err := unmarshalAt(r, bi, ftyp); err == nil {
				fa.MajorBrand = trimBrand(ftyp.MajorBrand)
			}
		case mp4.BoxTypeMoov():
			moovSeen = true
		case mp4.BoxTypeMdat():
			start := bi.Offset + bi.HeaderSize
			end := bi.Offset + bi.Size
			if end > uint64(len(data)) {
				// The box's declared size runs past EOF: the tail was cut.
				fa.Issues.Add(media.NewIssue(media.ClassMP4Truncated, media.Locator{
					ByteOffset: int64(end), ByteLength: int64(end) - int64(len(data)),
				}, "mdat declares %d bytes but only %d are present", end-start, int64(len(data))-int64(start)))
				end = uint64(len(data))
			}
			if start < end {
				mdatRanges = append(mdatRanges, byteRange{start: start, end: end})
			}
		case mp4.BoxTypeTkhd(), mp4.BoxTypeMdhd(), mp4.BoxTypeHdlr(), mp4.BoxTypeStsd(),
			mp4.BoxTypeStts(), mp4.BoxTypeStsc(), mp4.BoxTypeStsz(), mp4.BoxTypeStco(),
			mp4.BoxTypeCo64(), mp4.BoxTypeStss():
			t := findTrak(bi.Offset)
			if t == nil {
				continue
			}
			switch bi.Type {
			case mp4.BoxTypeTkhd():
				t.tkhd = bi
			case mp4.BoxTypeMdhd():
				t.mdhd = bi
			case mp4.BoxTypeHdlr():
				t.hdlr = bi
			case mp4.BoxTypeStsd():
				t.stsd = bi
			case mp4.BoxTypeStts():
				t.stts = bi
			case mp4.BoxTypeStsc():
				t.stsc = bi
			case mp4.BoxTypeStsz():
				t.stsz = bi
			case mp4.BoxTypeStco():
				t.stco = bi
			case mp4.BoxTypeCo64():
				t.co64 = bi
			case mp4.BoxTypeStss():
				t.stss = bi
			}
		}
	}
	if !moovSeen {
		fa.Issues.Add(media.NewFatal(media.ClassMP4BadStructure, media.Locator{}, "no moov box found"))
		return fa, nil
	}
	fa.TrackCount = len(traks)
	for _, t := range traks {
		track, issues, corrupt := analyzeTrack(r, t, mdatRanges, limits)
		fa.Issues.Add(issues...)
		fa.CorruptRanges = append(fa.CorruptRanges, corrupt...)
		if track != nil {
			fa.Tracks = append(fa.Tracks, track)
		}
	}
	fa.Issues.Sort()
	fa.CorruptRanges = media.MergeRanges(fa.CorruptRanges)
	return fa, nil
}

// byteRange is a half-open absolute byte span.
type byteRange struct{ start, end uint64 }

func (b byteRange) contains(off, size uint64) bool {
	return off >= b.start && off+size <= b.end
}

func unmarshalAt(r io.ReadSeeker, bi *mp4.BoxInfo, dst mp4.IBox) error {
	if _, err := bi.SeekToPayload(r); err != nil {
		return err
	}
	_, err := mp4.Unmarshal(r, bi.Size-bi.HeaderSize, dst, bi.Context)
	return err
}

func trimBrand(b [4]byte) string {
	s := string(b[:])
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}
