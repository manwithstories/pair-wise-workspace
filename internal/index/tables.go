package index

import (
	"os"

	"github.com/abema/go-mp4"

	"mediad/internal/model"
)

// readChunkOffsets returns the flat chunk offset table, preferring co64.
func readChunkOffsets(f *os.File, stbl *mp4.BoxInfo, idx *FileIndex) (
	[]uint64, uint32, *mp4.BoxInfo, error) {
	if boxes, _ := mp4.ExtractBoxWithPayload(f, stbl,
		mp4.BoxPath{mp4.BoxTypeCo64()}); len(boxes) > 0 {
		co := boxes[0].Payload.(*mp4.Co64)
		return append([]uint64(nil), co.ChunkOffset...), co.EntryCount,
			&boxes[0].Info, nil
	}
	boxes, err := mp4.ExtractBoxWithPayload(f, stbl, mp4.BoxPath{mp4.BoxTypeStco()})
	if err != nil {
		return nil, 0, nil, err
	}
	if len(boxes) == 0 {
		return nil, 0, nil, model.NewFault(model.FaultMissingIndex,
			int64(stbl.Offset), idx.Name, "stbl missing stco/co64")
	}
	stco := boxes[0].Payload.(*mp4.Stco)
	out := make([]uint64, len(stco.ChunkOffset))
	for i, v := range stco.ChunkOffset {
		out[i] = uint64(v)
	}
	return out, stco.EntryCount, &boxes[0].Info, nil
}

// expandStts flattens the run-length encoded decode delta table.
func expandStts(stts *mp4.Stts) []uint32 {
	n := 0
	for _, e := range stts.Entries {
		n += int(e.SampleCount)
	}
	out := make([]uint32, 0, n)
	for _, e := range stts.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			out = append(out, e.SampleDelta)
		}
	}
	return out
}

// readCtts expands composition-time offsets as signed per-sample values
// (version 1 boxes carry signed offsets). Absent ctts yields nil.
func readCtts(f *os.File, stbl *mp4.BoxInfo) []int64 {
	boxes, err := mp4.ExtractBoxWithPayload(f, stbl, mp4.BoxPath{mp4.BoxTypeCtts()})
	if err != nil || len(boxes) == 0 {
		return nil
	}
	ctts := boxes[0].Payload.(*mp4.Ctts)
	var out []int64
	for _, e := range ctts.Entries {
		var off int64
		if ctts.Version == 1 {
			off = int64(e.SampleOffsetV1)
		} else {
			off = int64(e.SampleOffsetV0)
		}
		for i := uint32(0); i < e.SampleCount; i++ {
			out = append(out, off)
		}
	}
	return out
}

// readKeyframes returns a per-sample keyframe map. Without stss every
// sample is treated as a keyframe.
func readKeyframes(f *os.File, stbl *mp4.BoxInfo, idx *FileIndex) map[uint32]bool {
	out := make(map[uint32]bool)
	boxes, err := mp4.ExtractBoxWithPayload(f, stbl, mp4.BoxPath{mp4.BoxTypeStss()})
	if err != nil || len(boxes) == 0 {
		return nil
	}
	stss := boxes[0].Payload.(*mp4.Stss)
	for _, n := range stss.SampleNumber {
		if n > 0 {
			out[n-1] = true
		}
	}
	return out
}

func sampleSize(stsz *mp4.Stsz, i uint32) (uint32, bool) {
	if stsz.SampleSize > 0 {
		return stsz.SampleSize, true
	}
	if int(i) >= len(stsz.EntrySize) {
		return 0, false
	}
	return stsz.EntrySize[i], true
}

// signedNano converts a signed timescale-unit offset to the nano base.
func signedNano(off int64, timescale uint32) model.Timestamp {
	if off >= 0 {
		return model.ToNanoseconds(uint64(off), timescale)
	}
	return -model.ToNanoseconds(uint64(-off), timescale)
}

// detectPTSFaults classifies PTS discontinuities on native timescale units.
// A backward jump explained by the 32-bit modular boundary is a wraparound;
// any other backward jump is a true regression. The first occurrence of
// each kind is recorded with its sample index and byte offset.
func detectPTSFaults(samples []Sample, rawPTS []int64, tr *Track,
	idx *FileIndex, addFault faultAdder) {
	const mod = int64(1) << 32
	if len(samples) > 0 {
		tr.FirstPTS = int64(samples[0].PTS)
	}
	// unwrap modular values to obtain a monotonic reference
	unwrapped := make([]int64, len(rawPTS))
	var acc int64
	for i, v := range rawPTS {
		if i > 0 {
			prev := rawPTS[i-1]
			if v < prev && prev-v > mod/2 {
				acc += mod // forward wrap across the 32-bit boundary
			}
		}
		unwrapped[i] = v + acc
	}
	for i := 1; i < len(rawPTS); i++ {
		if rawPTS[i] >= rawPTS[i-1] {
			continue
		}
		prev := rawPTS[i-1]
		isWrap := prev-rawPTS[i] > mod/2
		if isWrap && tr.WrappedAt == -1 {
			tr.WrappedAt = i
			addFault(model.NewFault(model.FaultPTSWrap, samples[i].Offset,
				idx.Name, "PTS 32-bit wraparound at sample "+itoa(i)))
			continue
		}
		if !isWrap && tr.Regression == -1 {
			tr.Regression = i
			addFault(model.NewFault(model.FaultPTSRegression, samples[i].Offset,
				idx.Name, "PTS regresses at sample "+itoa(i)))
		}
	}
	// a regression can also hide inside an unwrapped sequence
	for i := 1; i < len(unwrapped); i++ {
		if unwrapped[i] < unwrapped[i-1] && tr.Regression == -1 {
			tr.Regression = i
			addFault(model.NewFault(model.FaultPTSRegression, samples[i].Offset,
				idx.Name, "PTS regresses at sample "+itoa(i)))
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
