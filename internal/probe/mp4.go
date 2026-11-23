package probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediaroute/internal/fault"
)

// supportedCodecs is the set of sample entry fourCCs the pipeline can
// interpret fully. Unknown codecs are scanned structurally and reported.
var supportedCodecs = map[string]bool{
	"avc1": true, "avc3": true, "hvc1": true, "hev1": true, "mp4a": true,
}

type mp4TrackBuild struct {
	trackID uint32
	tkhd    *mp4.Tkhd
	mdhd    *mp4.Mdhd
	handler string
	codec   string
	stsz    *mp4.Stsz
	stsc    *mp4.Stsc
	stts    *mp4.Stts
	stco    []int64
	stss    map[uint32]bool
	colr    *Colour
	crop    *CropRect
}

// clapBox models the ISOBMFF clean-aperture box, which go-mp4 v1.3.0 does
// not register. BeforeUnmarshal consumes the full payload so parent
// traversal stays aligned; the probe parses the payload via ReadData.
type clapBox struct {
	mp4.Box
	Payload []byte
}

func (*clapBox) GetType() mp4.BoxType { return mp4.StrToBoxType("clap") }

func (c *clapBox) BeforeUnmarshal(r io.ReadSeeker, size uint64,
	_ mp4.Context) (uint64, bool, error) {
	c.Payload = make([]byte, size)
	if _, err := io.ReadFull(r, c.Payload); err != nil {
		return 0, true, err
	}
	return size, true, nil
}

// rawSampleEntry is a generic sample-entry shell for fourCCs go-mp4 v1.3.0
// does not model (e.g. mp4v). It absorbs the whole entry including nested
// child boxes; the probe still classifies the codec as unsupported itself.
type rawSampleEntry struct {
	mp4.AnyTypeBox
	Payload []byte
}

func (e *rawSampleEntry) GetType() mp4.BoxType { return e.Type }

func (e *rawSampleEntry) BeforeUnmarshal(r io.ReadSeeker, size uint64,
	_ mp4.Context) (uint64, bool, error) {
	e.Payload = make([]byte, size)
	if _, err := io.ReadFull(r, e.Payload); err != nil {
		return 0, true, err
	}
	return size, true, nil
}

func init() {
	mp4.AddBoxDef(&clapBox{}, 0)
	for _, cc := range []string{"mp4v", "av01", "vp09"} {
		mp4.AddBoxDef(&rawSampleEntry{AnyTypeBox: mp4.AnyTypeBox{
			Type: mp4.StrToBoxType(cc),
		}}, 0)
	}
}

// ScanMP4 scans an ISOBMFF/MP4 container into a ContainerReport. All
// structural problems are returned as structured fault.Error values.
func ScanMP4(path string) (*ContainerReport, error) {
	raw, err := readBounded(path)
	if err != nil {
		return nil, err
	}
	rep := &ContainerReport{
		Format:      FormatMP4,
		Path:        path,
		InputSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)),
		SizeBytes:   int64(len(raw)),
		KeyStats:    map[uint32]KeyframeStats{},
	}
	r := bytes.NewReader(raw)
	b := &mp4Builder{}
	if _, err := mp4.ReadBoxStructure(r, b.handle); err != nil {
		if b.err != nil {
			return nil, b.err
		}
		track := uint32(0)
		if len(b.tracks) > 0 {
			track = b.lastTrackID()
		}
		return nil, fault.New(fault.CatTruncatedSampleTable,
			"malformed MP4 structure: %v", err).WithTrack(track)
	}
	if len(b.tracks) == 0 {
		return nil, fault.New(fault.CatTruncatedSampleTable, "no tracks found")
	}
	for _, tb := range b.tracks {
		tm, ferr := tb.expand(raw, rep, b.containerColr)
		if ferr != nil {
			return nil, ferr
		}
		rep.KeyStats[tm.TrackID] = ComputeKeyframeStats(tm)
		rep.Tracks = append(rep.Tracks, *tm)
	}
	return rep, nil
}

func readBounded(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fault.New(fault.CatIO, "stat %s: %v", path, err)
	}
	if st.Size() > MaxFileSize {
		return nil, fault.New(fault.CatLimitExceeded,
			"file %d bytes exceeds limit %d", st.Size(), MaxFileSize)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.New(fault.CatIO, "read %s: %v", path, err)
	}
	return raw, nil
}

type mp4Builder struct {
	tracks        []*mp4TrackBuild
	containerColr *Colour
	err           *fault.Error
}

// lastTrackID returns the best-known track id after a parse failure.
// tkhd may not have been read yet, in which case trak order (1-based)
// is used as a stable locator.
func (b *mp4Builder) lastTrackID() uint32 {
	tb := b.tracks[len(b.tracks)-1]
	if tb.trackID != 0 {
		return tb.trackID
	}
	return uint32(len(b.tracks))
}

func (b *mp4Builder) handle(h *mp4.ReadHandle) (interface{}, error) {
	path := h.Path
	last := path[len(path)-1].String()
	if last == "trak" {
		b.tracks = append(b.tracks, &mp4TrackBuild{stss: map[uint32]bool{}})
		return h.Expand()
	}
	inTrack := false
	for _, t := range path {
		if t.String() == "trak" {
			inTrack = true
		}
	}
	if last == "colr" && !inTrack {
		box, _, e := h.ReadPayload()
		if e == nil {
			if c, ok := box.(*mp4.Colr); ok {
				b.containerColr = colrFromMP4(c)
			}
		}
		return h.Expand()
	}
	if !inTrack {
		return h.Expand()
	}
	tb := b.tracks[len(b.tracks)-1]
	if parentIs(path, "stsd") {
		if supportedCodecs[last] {
			tb.codec = last
		} else if tb.codec == "" {
			tb.codec = last
		}
		return h.Expand()
	}
	switch last {
	case "tkhd", "mdhd", "hdlr", "stsz", "stsc", "stts", "stco", "co64", "stss", "colr":
		box, _, e := h.ReadPayload()
		if e != nil {
			b.err = fault.New(fault.CatTruncatedSampleTable,
				"box %s payload truncated: %v", last, e).
				WithTrack(tb.trackID)
			return nil, b.err
		}
		tb.consume(last, box)
	case "clap":
		var buf bytes.Buffer
		if _, e := h.ReadData(&buf); e != nil {
			b.err = fault.New(fault.CatTruncatedSampleTable,
				"clap box truncated").WithTrack(tb.trackID)
			return nil, b.err
		}
		tb.crop = parseClap(buf.Bytes())
	}
	return h.Expand()
}

func parentIs(path mp4.BoxPath, want string) bool {
	return len(path) >= 2 && path[len(path)-2].String() == want
}

func colrFromMP4(c *mp4.Colr) *Colour {
	return &Colour{
		Primaries: c.ColourPrimaries, Transfer: c.TransferCharacteristics,
		Matrix: c.MatrixCoefficients, FullRange: c.FullRangeFlag,
	}
}

// parseClap decodes a clap payload (FullBox header + 8 uint32 rationals)
// into a pixel rectangle. Offsets are assumed centred at zero.
func parseClap(p []byte) *CropRect {
	if len(p) < 36 {
		return nil
	}
	u := p[4:]
	wd := binary.BigEndian.Uint32(u[4:])
	ht := binary.BigEndian.Uint32(u[12:])
	if wd == 0 || ht == 0 {
		return nil
	}
	cleanW := binary.BigEndian.Uint32(u[0:]) / wd
	cleanH := binary.BigEndian.Uint32(u[8:]) / ht
	return &CropRect{Right: cleanW, Bottom: cleanH}
}

func (tb *mp4TrackBuild) consume(kind string, box mp4.IBox) {
	switch v := box.(type) {
	case *mp4.Tkhd:
		tb.trackID, tb.tkhd = v.TrackID, v
	case *mp4.Mdhd:
		tb.mdhd = v
	case *mp4.Hdlr:
		tb.handler = string(v.HandlerType[:])
	case *mp4.Stsz:
		tb.stsz = v
	case *mp4.Stsc:
		tb.stsc = v
	case *mp4.Stts:
		tb.stts = v
	case *mp4.Stco:
		for _, o := range v.ChunkOffset {
			tb.stco = append(tb.stco, int64(o))
		}
	case *mp4.Co64:
		for _, o := range v.ChunkOffset {
			tb.stco = append(tb.stco, int64(o))
		}
	case *mp4.Stss:
		for _, n := range v.SampleNumber {
			tb.stss[n] = true
		}
	case *mp4.Colr:
		tb.colr = colrFromMP4(v)
	}
}
