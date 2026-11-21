package metadata

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediasync/internal/model"
)

// boxNode is the minimal ISO BMFF box tree used by the supported subset.
type boxNode struct {
	info     *mp4.BoxInfo
	payload  []byte // nil for container boxes
	children []*boxNode
}

// readBoxTree walks an ISO BMFF byte stream using go-mp4's header parser.
// Unsupported box types (e.g. clap/frta in this subset) keep raw payloads so
// they can be interpreted manually instead of failing the parse.
func readBoxTree(r io.ReadSeeker, end int64) ([]*boxNode, error) {
	var out []*boxNode
	for {
		pos, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		if pos >= end {
			return out, nil
		}
		bi, err := mp4.ReadBoxInfo(r)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading box at offset %d: %w", pos, err)
		}
		node := &boxNode{info: bi}
		boxEnd := int64(bi.Offset + bi.Size)
		switch {
		case bi.IsSupportedType() && isContainer(bi.Type):
			if _, err := bi.SeekToPayload(r); err != nil {
				return nil, err
			}
			node.children, err = readBoxTree(r, boxEnd)
			if err != nil {
				return nil, err
			}
		default:
			if _, err := bi.SeekToPayload(r); err != nil {
				return nil, err
			}
			size := bi.Size - bi.HeaderSize
			if hdr, ok := sampleEntryHeader(bi.Type.String()); ok && size >= uint64(hdr) {
				// Visual/audio sample entries: fixed header followed by child boxes.
				if _, err := r.Seek(int64(bi.Offset+bi.HeaderSize)+hdr, io.SeekStart); err != nil {
					return nil, err
				}
				node.children, err = readBoxTree(r, boxEnd)
				if err != nil {
					return nil, err
				}
			} else {
				buf := make([]byte, size)
				if _, err := io.ReadFull(r, buf); err != nil {
					return nil, err
				}
				node.payload = buf
			}
		}
		if _, err := bi.SeekToEnd(r); err != nil {
			return nil, err
		}
		out = append(out, node)
	}
}

func isContainer(t mp4.BoxType) bool {
	name := t.String()
	switch name {
	case "moov", "trak", "mdia", "minf", "stbl", "udta":
		return true
	}
	return false
}

// sampleEntryHeader reports the fixed header size of the sample-entry box types
// in the supported subset; child boxes begin immediately after it.
func sampleEntryHeader(fourcc string) (int64, bool) {
	switch fourcc {
	case "avc1", "hev1", "hvc1", "mp4v", "encv":
		return 78, true // SampleEntry(8) + VisualSampleEntry fixed fields(70)
	case "mp4a":
		return 28, true
	default:
		return 0, false
	}
}

// findAll returns descendant nodes matching the 4CC, at any nesting depth.
func (n *boxNode) findAll(fourcc string) []*boxNode {
	var out []*boxNode
	if n.info.Type.String() == fourcc {
		out = append(out, n)
	}
	for _, c := range n.children {
		out = append(out, c.findAll(fourcc)...)
	}
	return out
}

func (n *boxNode) findOne(fourcc string) *boxNode {
	all := n.findAll(fourcc)
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

// ParseMP4 parses the supported MP4 metadata subset from a self-generated
// sample. Payload decoding uses go-mp4 for standardized boxes (tkhd/mdhd/
// hdlr/stts/colr); clap and the synthetic frta box are decoded from raw bytes.
func ParseMP4(r io.ReadSeeker, source string) (*ParseResult, error) {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	root, err := readBoxTree(r, end)
	if err != nil {
		return nil, err
	}
	res := &ParseResult{}
	for _, node := range root {
		if node.info.Type.String() != "moov" {
			continue
		}
		for _, trak := range node.findAll("trak") {
			t := decodeTrak(trak, source, &res.Diag)
			if t != nil {
				finalizeTrack(t, &res.Diag)
				res.Tracks = append(res.Tracks, *t)
			}
		}
	}
	if len(res.Tracks) == 0 {
		return nil, fmt.Errorf("%s: no trak boxes found", source)
	}
	return res, nil
}

func decodeTrak(trak *boxNode, source string, diag *model.Bag) *TrackMeta {
	t := &TrackMeta{Source: source}
	if hdlr := trak.findOne("hdlr"); hdlr != nil {
		var h mp4.Hdlr
		if _, err := mp4.Unmarshal(bytes.NewReader(hdlr.payload), uint64(len(hdlr.payload)), &h, mp4.Context{}); err != nil {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: model.NoTimestamp, Message: "hdlr decode: " + err.Error()})
		} else {
			switch string(h.HandlerType[:]) {
			case "vide":
				t.Kind = model.KindVideo
			case "soun":
				t.Kind = model.KindAudio
			default:
				diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
					Track: t.TrackID, AtTicks: model.NoTimestamp,
					Message: "unsupported handler " + string(h.HandlerType[:])})
			}
		}
	}
	if tkhd := trak.findOne("tkhd"); tkhd != nil {
		var h mp4.Tkhd
		if _, err := mp4.Unmarshal(bytes.NewReader(tkhd.payload), uint64(len(tkhd.payload)), &h, mp4.Context{}); err != nil {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: model.NoTimestamp, Message: "tkhd decode: " + err.Error()})
		} else {
			t.TrackID = fmt.Sprintf("mp4-%d", h.TrackID)
			t.Width = int64(h.Width >> 16)
			t.Height = int64(h.Height >> 16)
			angle, ok := matrixRotation(h.Matrix)
			if !ok {
				diag.Add(model.Diagnostic{Kind: model.KindBadRotation, Severity: model.SeverityWarning,
					Track: t.TrackID, AtTicks: model.NoTimestamp, Recoverable: true,
					Message: "non-cardinal tkhd matrix; rotation reset to 0"})
				angle = 0
			}
			t.HasRot, t.Rotation = true, angle
		}
	}
	if mdhd := trak.findOne("mdhd"); mdhd != nil {
		var h mp4.Mdhd
		if _, err := mp4.Unmarshal(bytes.NewReader(mdhd.payload), uint64(len(mdhd.payload)), &h, mp4.Context{}); err != nil {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: model.NoTimestamp, Message: "mdhd decode: " + err.Error()})
		} else {
			t.Timescale = int64(h.Timescale)
			if h.GetVersion() == 1 {
				t.Start = model.Rational{Num: int64(h.CreationTimeV1), Den: 1}.Reduce()
			}
		}
	}
	if stts := trak.findOne("stts"); stts != nil {
		var s mp4.Stts
		if _, err := mp4.Unmarshal(bytes.NewReader(stts.payload), uint64(len(stts.payload)), &s, mp4.Context{}); err != nil {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityError,
				Track: t.TrackID, AtTicks: model.NoTimestamp, Message: "stts decode: " + err.Error()})
		} else if len(s.Entries) > 0 {
			e := s.Entries[0]
			if e.SampleDelta > 0 && t.Timescale > 0 {
				t.FrameRate = model.Rational{Num: int64(t.Timescale), Den: int64(e.SampleDelta)}.Reduce()
			}
		}
	}
	if frta := trak.findOne("frta"); frta != nil {
		if r, ok := decodeFrta(frta.payload); ok {
			t.FrameRate = r
		} else {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityWarning,
				Track: t.TrackID, AtTicks: model.NoTimestamp, Message: "frta box malformed"})
		}
	}
	if clap := trak.findOne("clap"); clap != nil {
		if c, ok := decodeClap(clap.payload); ok {
			t.Crop = c
		} else {
			diag.Add(model.Diagnostic{Kind: model.KindIllegalField, Severity: model.SeverityWarning,
				Track: t.TrackID, AtTicks: model.NoTimestamp, Message: "clap box malformed"})
		}
	}
	if colr := trak.findOne("colr"); colr != nil {
		var c mp4.Colr
		if _, err := mp4.Unmarshal(bytes.NewReader(colr.payload), uint64(len(colr.payload)), &c, mp4.Context{}); err != nil {
			diag.Add(model.Diagnostic{Kind: model.KindColorConflict, Severity: model.SeverityWarning,
				Track: t.TrackID, AtTicks: model.NoTimestamp, Message: "colr decode: " + err.Error()})
		} else {
			t.Color = &ColorDesc{ColorType: string(c.ColourType[:]), Primaries: c.ColourPrimaries,
				Transfer: c.TransferCharacteristics, Matrix: c.MatrixCoefficients, FullRange: c.FullRangeFlag}
		}
	}
	return t
}

// matrixRotation extracts a cardinal clockwise rotation from a 3x3 fixed-point
// matrix. Non-cardinal or invalid matrices report ok=false.
func matrixRotation(m [9]int32) (int64, bool) {
	const one = 1 << 16
	want := map[int][4]int32{
		0:   {one, 0, 0, one},
		90:  {0, one, -one, 0},
		180: {-one, 0, 0, -one},
		270: {0, -one, one, 0},
	}
	got := [4]int32{m[0], m[1], m[3], m[4]}
	for deg, w := range want {
		if got == w {
			return int64(deg), true
		}
	}
	return 0, false
}

// decodeClap reads the eight 32-bit fixed-point clean aperture fields and
// rounds them to integer pixel dimensions.
func decodeClap(b []byte) (*CropRect, bool) {
	if len(b) < 32 {
		return nil, false
	}
	rd := func(i int) int64 {
		v := int64(int32(binary.BigEndian.Uint32(b[i*4:])))
		return (v + (1 << 15)) >> 16
	}
	w, h := rd(0), rd(2)
	if w <= 0 || h <= 0 {
		return nil, false
	}
	x := rd(4) // clean aperture center offset, rounded to pixels
	y := rd(6)
	return &CropRect{X: x, Y: y, Width: w, Height: h}, true
}

// decodeFrta reads the synthetic frame-rate box: num(32) den(32) start(32/32).
func decodeFrta(b []byte) (model.Rational, bool) {
	if len(b) < 16 {
		return model.Rational{}, false
	}
	num := binary.BigEndian.Uint32(b[0:])
	den := binary.BigEndian.Uint32(b[4:])
	if num == 0 || den == 0 {
		return model.Rational{}, false
	}
	return model.Rational{Num: int64(num), Den: int64(den)}, true
}

// boxWriter assembles ISO BMFF boxes with big-endian 32-bit headers.
type boxWriter struct{ buf bytes.Buffer }

func (w *boxWriter) raw(fourcc string, payload []byte) {
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(8+len(payload)))
	copy(hdr[4:], []byte(fourcc))
	w.buf.Write(hdr)
	w.buf.Write(payload)
}

func (w *boxWriter) container(fourcc string, children []byte) { w.raw(fourcc, children) }

// WriteMP4 builds a minimal but valid ISO BMFF byte stream containing the
// metadata subset. go-mp4's own reader must be able to decode every standard box.
func WriteMP4(wr io.Writer, tracks []TrackMeta) error {
	var root boxWriter
	var ftyp bytes.Buffer
	ftyp.WriteString("isom")
	binary.Write(&ftyp, binary.BigEndian, uint32(0))
	ftyp.WriteString("isomiso2")
	root.raw("ftyp", ftyp.Bytes())

	var moov bytes.Buffer
	for idx, t := range tracks {
		var trak bytes.Buffer
		// tkhd
		var tkhd bytes.Buffer
		binary.Write(&tkhd, binary.BigEndian, uint8(0))  // version
		tkhd.Write(make([]byte, 3))                      // flags
		binary.Write(&tkhd, binary.BigEndian, uint32(1)) // creation
		binary.Write(&tkhd, binary.BigEndian, uint32(0)) // modification
		binary.Write(&tkhd, binary.BigEndian, uint32(idx+1))
		binary.Write(&tkhd, binary.BigEndian, uint32(0)) // reserved
		binary.Write(&tkhd, binary.BigEndian, uint32(0)) // duration
		tkhd.Write(make([]byte, 8))                      // reserved
		binary.Write(&tkhd, binary.BigEndian, int16(0))  // layer/alt group
		binary.Write(&tkhd, binary.BigEndian, int16(0))
		vol := int16(0)
		if t.Kind == model.KindAudio {
			vol = 0x0100
		}
		binary.Write(&tkhd, binary.BigEndian, vol)
		binary.Write(&tkhd, binary.BigEndian, uint16(0))
		writeMatrix(&tkhd, t.Rotation)
		binary.Write(&tkhd, binary.BigEndian, uint32(t.Width<<16))
		binary.Write(&tkhd, binary.BigEndian, uint32(t.Height<<16))
		writeBox(&trak, "tkhd", tkhd.Bytes())

		// mdia -> mdhd + hdlr + minf/stbl(stts, sample entry with colr/clap/frta)
		var mdia bytes.Buffer
		var mdhd bytes.Buffer
		mdhd.WriteByte(0)
		mdhd.Write(make([]byte, 3))
		binary.Write(&mdhd, binary.BigEndian, uint32(0))
		binary.Write(&mdhd, binary.BigEndian, uint32(0))
		binary.Write(&mdhd, binary.BigEndian, uint32(t.Timescale))
		binary.Write(&mdhd, binary.BigEndian, uint32(0))
		mdhd.Write([]byte{0x55, 0xc4}) // und language
		binary.Write(&mdhd, binary.BigEndian, uint16(0))
		writeBox(&mdia, "mdhd", mdhd.Bytes())

		var hdlr bytes.Buffer
		hdlr.Write(make([]byte, 4))
		binary.Write(&hdlr, binary.BigEndian, uint32(0))
		if t.Kind == model.KindAudio {
			hdlr.WriteString("soun")
		} else {
			hdlr.WriteString("vide")
		}
		hdlr.Write(make([]byte, 12))
		hdlr.WriteString("mediasync")
		hdlr.WriteByte(0)
		writeBox(&mdia, "hdlr", hdlr.Bytes())

		var stbl bytes.Buffer
		var stts bytes.Buffer
		stts.Write(make([]byte, 4))
		binary.Write(&stts, binary.BigEndian, uint32(1))
		delta := t.Timescale * t.FrameRate.Den / t.FrameRate.Num
		binary.Write(&stts, binary.BigEndian, uint32(1))
		binary.Write(&stts, binary.BigEndian, uint32(delta))
		writeBox(&stbl, "stts", stts.Bytes())

		var entry bytes.Buffer
		entry.Write(make([]byte, 6))
		binary.Write(&entry, binary.BigEndian, uint16(1)) // data ref index
		entry.Write(make([]byte, 16))                     // reserved/predefined
		binary.Write(&entry, binary.BigEndian, uint16(t.Width))
		binary.Write(&entry, binary.BigEndian, uint16(t.Height))
		entry.Write(make([]byte, 50)) // compressor/resolution/depth leftovers
		if t.Color != nil {
			var colr bytes.Buffer
			colr.WriteString("nclx")
			binary.Write(&colr, binary.BigEndian, t.Color.Primaries)
			binary.Write(&colr, binary.BigEndian, t.Color.Transfer)
			binary.Write(&colr, binary.BigEndian, t.Color.Matrix)
			flag := byte(0)
			if t.Color.FullRange {
				flag = 0x80
			}
			colr.WriteByte(flag)
			writeBox(&entry, "colr", colr.Bytes())
		}
		if t.Crop != nil {
			var clap bytes.Buffer
			writeFP16(&clap, t.Crop.Width)
			writeFP16(&clap, 1)
			writeFP16(&clap, t.Crop.Height)
			writeFP16(&clap, 1)
			writeFP16(&clap, t.Crop.X)
			writeFP16(&clap, 1)
			writeFP16(&clap, t.Crop.Y)
			writeFP16(&clap, 1)
			writeBox(&entry, "clap", clap.Bytes())
		}
		var frta bytes.Buffer
		binary.Write(&frta, binary.BigEndian, uint32(t.FrameRate.Num))
		binary.Write(&frta, binary.BigEndian, uint32(t.FrameRate.Den))
		binary.Write(&frta, binary.BigEndian, uint32(0))
		binary.Write(&frta, binary.BigEndian, uint32(0))
		writeBox(&entry, "frta", frta.Bytes())
		writeBox(&stbl, "avc1", entry.Bytes())

		var minf bytes.Buffer
		writeBox(&minf, "stbl", stbl.Bytes())
		writeBox(&mdia, "minf", minf.Bytes())
		writeBox(&trak, "mdia", mdia.Bytes())
		moov.Write(boxBytes("trak", trak.Bytes()))
	}
	root.container("moov", moov.Bytes())
	_, err := wr.Write(root.buf.Bytes())
	return err
}

func writeBox(w io.Writer, fourcc string, payload []byte) { w.Write(boxBytes(fourcc, payload)) }

func boxBytes(fourcc string, payload []byte) []byte {
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(8+len(payload)))
	copy(hdr[4:], fourcc)
	return append(hdr, payload...)
}

func writeFP16(w io.Writer, num int64) {
	binary.Write(w, binary.BigEndian, uint32(num<<16))
}

func writeMatrix(w io.Writer, angle int64) {
	const one = int32(1 << 16)
	var m [9]int32
	switch angle {
	case 90:
		m = [9]int32{0, one, 0, -one, 0, 0, 0, 0, one}
	case 180:
		m = [9]int32{-one, 0, 0, 0, -one, 0, 0, 0, one}
	case 270:
		m = [9]int32{0, -one, 0, one, 0, 0, 0, 0, one}
	default:
		m = [9]int32{one, 0, 0, 0, one, 0, 0, 0, one}
	}
	for _, v := range m {
		binary.Write(w, binary.BigEndian, v)
	}
}
