package mp4

import (
	"bytes"
	"io"

	"github.com/abema/go-mp4"

	"mediasync/internal/media"
)

// MaxFileBytes caps the in-memory MP4 fixture size accepted by Index.
const MaxFileBytes = 64 << 20

// Index parses MP4 bytes and returns an index per track. Truncated or corrupt
// structures yield recoverable issues with byte offsets whenever the leading
// boxes were readable; a missing moov is fatal.
func Index(data []byte, file string) []*Track {
	if len(data) > MaxFileBytes {
		return []*Track{{File: file, Issues: []*media.Issue{fatalErr(file,
			"input is %d bytes, limit is %d", len(data), MaxFileBytes)}}}
	}
	tracks, err := buildTracks(bytes.NewReader(data), file, data)
	if err != nil {
		return []*Track{{File: file, Issues: []*media.Issue{fatalErr(file, "%s", describe(err))}}}
	}
	if len(tracks) == 0 {
		return []*Track{{File: file, Issues: []*media.Issue{fatalErr(file, "no tracks found (missing moov/trak)")}}}
	}
	return tracks
}

// trackBoxes collects the leaf boxes we need from one trak.
type trackBoxes struct {
	mdhd    *mp4.Mdhd
	hdlr    *mp4.Hdlr
	tkhd    *mp4.Tkhd
	stts    *mp4.Stts
	stss    *mp4.Stss
	stsz    *mp4.Stsz
	stsc    *mp4.Stsc
	stco    *mp4.Stco
	co64    *mp4.Co64
	stszOff uint64
}

var leafPaths = []mp4.BoxPath{
	{mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()},
	{mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()},
	{mp4.BoxTypeTkhd()},
	{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStts()},
	{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStss()},
	{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()},
	{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsc()},
	{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStco()},
	{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeCo64()},
}

func buildTracks(rs io.ReadSeeker, file string, data []byte) ([]*Track, error) {
	moovs, err := mp4.ExtractBoxesWithPayload(rs, nil, []mp4.BoxPath{{mp4.BoxTypeMoov()}})
	if err != nil {
		return nil, err
	}
	if len(moovs) == 0 {
		return nil, nil
	}
	traks, err := mp4.ExtractBoxesWithPayload(rs, &moovs[0].Info, []mp4.BoxPath{{mp4.BoxTypeTrak()}})
	if err != nil {
		return nil, err
	}
	tracks := make([]*Track, 0, len(traks))
	for _, trak := range traks {
		boxes, err := readTrackBoxes(rs, &trak.Info)
		tr := &Track{File: file}
		if err != nil {
			tr.Issues = append(tr.Issues, media.New(media.StageMP4, media.InputMP4, media.CodeTruncated,
				"track boxes unreadable: %s", describe(err)).
				WithFile(file).WithOffset(int64(trak.Info.Offset)).WithRecord(len(tracks)+1))
			tracks = append(tracks, tr)
			continue
		}
		tr.TrackID = boxes.trackID()
		tr.Handler = boxes.handler()
		tr.Timescale = boxes.timescale()
		tr.DurationNS = boxes.durationNS()
		tracks = append(tracks, tr)
		indexTrackSamples(tr, boxes, data)
	}
	return tracks, nil
}

func readTrackBoxes(rs io.ReadSeeker, trak *mp4.BoxInfo) (*trackBoxes, error) {
	bs, err := mp4.ExtractBoxesWithPayload(rs, trak, leafPaths)
	if err != nil {
		return nil, err
	}
	tb := &trackBoxes{}
	for _, b := range bs {
		if v, ok := b.Payload.(*mp4.Stsz); ok {
			tb.stszOff = b.Info.Offset
			tb.stsz = v
			continue
		}
		switch v := b.Payload.(type) {
		case *mp4.Mdhd:
			tb.mdhd = v
		case *mp4.Hdlr:
			tb.hdlr = v
		case *mp4.Tkhd:
			tb.tkhd = v
		case *mp4.Stts:
			tb.stts = v
		case *mp4.Stss:
			tb.stss = v
		case *mp4.Stsc:
			tb.stsc = v
		case *mp4.Stco:
			tb.stco = v
		case *mp4.Co64:
			tb.co64 = v
		}
	}
	return tb, nil
}

func (tb *trackBoxes) trackID() uint32 {
	if tb.tkhd != nil {
		return tb.tkhd.TrackID
	}
	return 0
}

func (tb *trackBoxes) handler() string {
	if tb.hdlr == nil {
		return ""
	}
	return string(tb.hdlr.HandlerType[:])
}

func (tb *trackBoxes) timescale() uint32 {
	if tb.mdhd == nil {
		return 0
	}
	return tb.mdhd.Timescale
}

func (tb *trackBoxes) durationNS() int64 {
	if tb.mdhd == nil {
		return 0
	}
	var d uint64
	if tb.mdhd.GetVersion() == 1 {
		d = tb.mdhd.DurationV1
	} else {
		d = uint64(tb.mdhd.DurationV0)
	}
	return translate(int64(d), tb.mdhd.Timescale)
}

// offsets returns the per-chunk file offsets, whichever table is present.
func (tb *trackBoxes) offsets() []int64 {
	if tb.stco != nil {
		out := make([]int64, len(tb.stco.ChunkOffset))
		for i, v := range tb.stco.ChunkOffset {
			out[i] = int64(v)
		}
		return out
	}
	if tb.co64 != nil {
		out := make([]int64, len(tb.co64.ChunkOffset))
		for i, v := range tb.co64.ChunkOffset {
			out[i] = int64(v)
		}
		return out
	}
	return nil
}
