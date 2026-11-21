package demux

import (
	"encoding/binary"

	"mediamerge/internal/model"
)

func parseSTTS(b Table) ([]sttsRun, error) {
	p := b.Payload
	if len(p) < 8 {
		return nil, derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short stts")
	}
	n := binary.BigEndian.Uint32(p[4:8])
	if uint64(len(p)-8) < uint64(n)*8 {
		return nil, derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+8, 0, false,
			"stts declares %d runs beyond payload", n)
	}
	runs := make([]sttsRun, n)
	for i := range runs {
		o := 8 + i*8
		runs[i] = sttsRun{binary.BigEndian.Uint32(p[o : o+4]), binary.BigEndian.Uint32(p[o+4 : o+8])}
	}
	return runs, nil
}

func parseCTTS(b Table) ([]cttsRun, error) {
	p := b.Payload
	if len(p) < 8 {
		return nil, derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short ctts")
	}
	ver := p[0]
	n := binary.BigEndian.Uint32(p[4:8])
	if uint64(len(p)-8) < uint64(n)*8 {
		return nil, derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+8, 0, false,
			"ctts declares %d runs beyond payload", n)
	}
	runs := make([]cttsRun, n)
	for i := range runs {
		o := 8 + i*8
		raw := binary.BigEndian.Uint32(p[o+4 : o+8])
		off := int32(raw)
		if ver == 0 {
			off = int32(raw)
		}
		runs[i] = cttsRun{binary.BigEndian.Uint32(p[o : o+4]), off}
	}
	return runs, nil
}

func parseSTSC(b Table) ([]stscRun, error) {
	p := b.Payload
	if len(p) < 8 {
		return nil, derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short stsc")
	}
	n := binary.BigEndian.Uint32(p[4:8])
	if uint64(len(p)-8) < uint64(n)*12 {
		return nil, derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+8, 0, false,
			"stsc declares %d entries beyond payload", n)
	}
	runs := make([]stscRun, n)
	for i := range runs {
		o := 8 + i*12
		runs[i] = stscRun{
			binary.BigEndian.Uint32(p[o : o+4]),
			binary.BigEndian.Uint32(p[o+4 : o+8]),
			binary.BigEndian.Uint32(p[o+8 : o+12]),
		}
	}
	return runs, nil
}

func parseSTSZ(b Table) ([]uint32, error) {
	p := b.Payload
	if len(p) < 12 {
		return nil, derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short stsz")
	}
	fixed := binary.BigEndian.Uint32(p[4:8])
	count := binary.BigEndian.Uint32(p[8:12])
	if fixed != 0 {
		out := make([]uint32, count)
		for i := range out {
			out[i] = fixed
		}
		return out, nil
	}
	if uint64(len(p)-12) < uint64(count)*4 {
		return nil, derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+12, 0, false,
			"stsz declares %d sizes beyond payload", count)
	}
	out := make([]uint32, count)
	for i := range out {
		out[i] = binary.BigEndian.Uint32(p[12+i*4 : 16+i*4])
	}
	return out, nil
}

func parseSTZ2(b Table) ([]uint32, error) {
	p := b.Payload
	if len(p) < 12 {
		return nil, derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short stz2")
	}
	field := p[7]
	count := binary.BigEndian.Uint32(p[8:12])
	out := make([]uint32, count)
	need := uint64(0)
	switch field {
	case 4:
		need = (uint64(count) + 1) / 2
	case 8:
		need = uint64(count)
	case 16:
		need = uint64(count) * 2
	default:
		return nil, derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+7, 0, false,
			"stz2 invalid field size %d", field)
	}
	if uint64(len(p)-12) < need {
		return nil, derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+12, 0, false,
			"stz2 declares %d sizes beyond payload", count)
	}
	for i := uint32(0); i < count; i++ {
		switch field {
		case 4:
			v := p[12+i/2]
			if i%2 == 0 {
				out[i] = uint32(v >> 4)
			} else {
				out[i] = uint32(v & 0x0F)
			}
		case 8:
			out[i] = uint32(p[12+i])
		case 16:
			out[i] = uint32(binary.BigEndian.Uint16(p[12+i*2 : 14+i*2]))
		}
	}
	return out, nil
}

func parseSTCO(b Table) ([]uint64, error) {
	p := b.Payload
	if len(p) < 8 {
		return nil, derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short stco")
	}
	n := binary.BigEndian.Uint32(p[4:8])
	if uint64(len(p)-8) < uint64(n)*4 {
		return nil, derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+8, 0, false,
			"stco declares %d offsets beyond payload", n)
	}
	out := make([]uint64, n)
	for i := range out {
		out[i] = uint64(binary.BigEndian.Uint32(p[8+i*4 : 12+i*4]))
	}
	return out, nil
}

func parseCO64(b Table) ([]uint64, error) {
	p := b.Payload
	if len(p) < 8 {
		return nil, derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short co64")
	}
	n := binary.BigEndian.Uint32(p[4:8])
	if uint64(len(p)-8) < uint64(n)*8 {
		return nil, derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+8, 0, false,
			"co64 declares %d offsets beyond payload", n)
	}
	out := make([]uint64, n)
	for i := range out {
		out[i] = binary.BigEndian.Uint64(p[8+i*8 : 16+i*8])
	}
	return out, nil
}

func parseSTSS(b Table) ([]uint32, error) {
	p := b.Payload
	if len(p) < 8 {
		return nil, derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short stss")
	}
	n := binary.BigEndian.Uint32(p[4:8])
	if uint64(len(p)-8) < uint64(n)*4 {
		return nil, derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+8, 0, false,
			"stss declares %d entries beyond payload", n)
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.BigEndian.Uint32(p[8+i*4 : 12+i*4])
	}
	return out, nil
}
