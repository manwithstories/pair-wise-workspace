package pipeline

import (
	"encoding/binary"
	"math"

	"mediad/internal/bmff"
	"mediad/internal/model"
	"mediad/internal/trackmap"
)

func outputFTYP() []byte {
	p := make([]byte, 16)
	putFourCC(p[0:4], "isom")
	binary.BigEndian.PutUint32(p[4:8], 0x200)
	putFourCC(p[8:12], "isom")
	putFourCC(p[12:16], "mp41")
	return bmff.NewBox("ftyp").WithPayload(p).Build()
}

func putFourCC(dst []byte, typ string) {
	for i := 0; i < 4; i++ {
		if i < len(typ) {
			dst[i] = typ[i]
		}
	}
}

// mdatHeader builds a 32- or 64-bit mdat header and returns the absolute
// first sample byte offset.
func mdatHeader(totalMedia int64) (header []byte, dataStart int64, use64 bool, err error) {
	ftypLen := int64(len(outputFTYP()))
	if totalMedia+8 <= math.MaxUint32 {
		h := make([]byte, 8)
		binary.BigEndian.PutUint32(h[0:4], uint32(8+totalMedia))
		putFourCC(h[4:8], "mdat")
		return h, ftypLen + 8, false, nil
	}
	if totalMedia+16 > math.MaxInt64 {
		return nil, 0, false, model.New(model.CodeUnsupported, model.StagePipeline, "media too large for mdat")
	}
	h := make([]byte, 16)
	binary.BigEndian.PutUint32(h[0:4], 1)
	putFourCC(h[4:8], "mdat")
	binary.BigEndian.PutUint64(h[8:16], uint64(16+totalMedia))
	return h, ftypLen + 16, true, nil
}

func buildOutputMoov(plan *trackmap.Plan, offsets [][]int64) ([]byte, error) {
	var traks []*bmff.Builder
	for ti, pt := range plan.Tracks {
		if ti >= len(offsets) || len(offsets[ti]) != len(pt.Samples) {
			return nil, model.New(model.CodeInternal, model.StagePipeline, "output offset table does not match plan")
		}
		traks = append(traks, buildOutputTrak(&pt, offsets[ti]))
	}
	return bmff.NewBox("moov").Add(traks...).Build(), nil
}

func buildOutputTrak(pt *trackmap.PlannedTrack, offsets []int64) *bmff.Builder {
	tkhdPayload := make([]byte, 80)
	binary.BigEndian.PutUint32(tkhdPayload[8:12], pt.OutputID)
	if pt.Handler == "vide" {
		binary.BigEndian.PutUint32(tkhdPayload[72:76], uint32(320)<<16)
		binary.BigEndian.PutUint32(tkhdPayload[76:80], uint32(240)<<16)
	}
	tkhd := bmff.NewFullBox("tkhd", 0, 0x000007, tkhdPayload)

	var duration uint32
	if n := len(pt.Samples); n > 0 {
		last := pt.Samples[n-1]
		duration = uint32(last.DTS + int64(last.DurationOut))
	}
	mdhdPayload := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhdPayload[8:12], pt.OutputTimescale)
	binary.BigEndian.PutUint32(mdhdPayload[12:16], duration)
	mdhd := bmff.NewFullBox("mdhd", 0, 0, mdhdPayload)
	hdlrPayload := make([]byte, 21)
	putFourCC(hdlrPayload[4:8], pt.Handler)
	copy(hdlrPayload[12:], "Mediad Remux\x00")
	hdlr := bmff.NewFullBox("hdlr", 0, 0, hdlrPayload)

	stsdPayload := make([]byte, 4+len(pt.SampleEntry))
	binary.BigEndian.PutUint32(stsdPayload[0:4], 1)
	copy(stsdPayload[4:], pt.SampleEntry)
	stsd := bmff.NewFullBox("stsd", 0, 0, stsdPayload)

	stbl := bmff.NewBox("stbl").
		Add(stsd).
		Add(bmff.NewFullBox("stts", 0, 0, encodeOutputSTTS(pt))).
		Add(bmff.NewFullBox("stsc", 0, 0, bmff.EncodeSTSCOne())).
		Add(bmff.NewFullBox("stsz", 0, 0, encodeOutputSTSZ(pt)))
	if ctts := encodeOutputCTTS(pt); ctts != nil {
		stbl.Add(bmff.NewFullBox("ctts", 1, 0, ctts))
	}
	stco, _ := encodeOutputChunkBox(offsets)
	stbl.Add(stco)

	return bmff.NewBox("trak").Add(tkhd, bmff.NewBox("mdia").Add(mdhd, hdlr, bmff.NewBox("minf").Add(stbl)))
}

func encodeOutputSTTS(pt *trackmap.PlannedTrack) []byte {
	n := len(pt.Samples)
	p := make([]byte, 4+8*n)
	binary.BigEndian.PutUint32(p[0:4], uint32(n))
	for i, s := range pt.Samples {
		binary.BigEndian.PutUint32(p[4+i*8:], 1)
		binary.BigEndian.PutUint32(p[8+i*8:], s.DurationOut)
	}
	return p
}

func encodeOutputCTTS(pt *trackmap.PlannedTrack) []byte {
	n := len(pt.Samples)
	nonzero := false
	for _, s := range pt.Samples {
		if s.PTS != s.DTS {
			nonzero = true
		}
	}
	if !nonzero {
		return nil
	}
	p := make([]byte, 4+8*n)
	binary.BigEndian.PutUint32(p[0:4], uint32(n))
	for i, s := range pt.Samples {
		delta := s.PTS - s.DTS
		binary.BigEndian.PutUint32(p[4+i*8:], 1)
		binary.BigEndian.PutUint32(p[8+i*8:], uint32(int32(delta)))
	}
	return p
}

func encodeOutputSTSZ(pt *trackmap.PlannedTrack) []byte {
	n := len(pt.Samples)
	uniform := n > 0
	var first uint32
	if n > 0 {
		first = pt.Samples[0].Size
	}
	for _, s := range pt.Samples {
		if s.Size != first {
			uniform = false
		}
	}
	if uniform {
		return bmff.EncodeSTSZUniform(uint32(n), first)
	}
	p := make([]byte, 8+4*n)
	binary.BigEndian.PutUint32(p[0:4], 0)
	binary.BigEndian.PutUint32(p[4:8], uint32(n))
	for i, s := range pt.Samples {
		binary.BigEndian.PutUint32(p[8+i*4:], s.Size)
	}
	return p
}

func encodeOutputChunkBox(offsets []int64) (*bmff.Builder, bool) {
	use64 := false
	for _, off := range offsets {
		if off > math.MaxUint32 {
			use64 = true
			break
		}
	}
	if use64 {
		p := make([]byte, 4+8*len(offsets))
		binary.BigEndian.PutUint32(p[0:4], uint32(len(offsets)))
		for i, off := range offsets {
			binary.BigEndian.PutUint64(p[4+i*8:], uint64(off))
		}
		return bmff.NewFullBox("co64", 0, 0, p), true
	}
	p := make([]byte, 4+4*len(offsets))
	binary.BigEndian.PutUint32(p[0:4], uint32(len(offsets)))
	for i, off := range offsets {
		binary.BigEndian.PutUint32(p[4+i*4:], uint32(off))
	}
	return bmff.NewFullBox("stco", 0, 0, p), false
}
