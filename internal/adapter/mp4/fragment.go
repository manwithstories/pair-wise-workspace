package mp4

import (
	"bytes"
	"encoding/binary"

	"mediacheck/internal/model"
)

type trunHold struct {
	info      trunInfo
	baseOff   int64 // trun payload（字段区）在分片 data 中的绝对偏移
}

type fragState struct {
	seq       uint32
	hasMfhd   bool
	seqOffset int64
	timescale uint32
	tfdt      int64
	hasTfdt   bool
	trafSeen  bool
	trackID   uint32
	truns     []trunHold
	moofEnd   int64
	boxes     []model.BoxSummary
}

// parseFragment 解析单个 fMP4 分片（moof+mdat），产出统一时基样本与区间。
func parseFragment(data []byte, maxBox int64, tracks map[uint32]model.TrackMeta) (model.Fragment, error) {
	r := bytes.NewReader(data)
	st := &fragState{}
	var mdatPayload []byte
	var mdatOffset int64
	err := walkBoxes(r, int64(len(data)), 0, func(rb rawBox, depth int) error {
		summarize(&st.boxes)(rb, depth)
		typ := rb.info.Type.String()
		switch typ {
		case "moof":
			st.moofEnd = int64(rb.info.Offset + rb.info.Size)
		case "mfhd":
			seq, ok := parseMfhd(rb.payload)
			if !ok {
				return &model.MediaError{Category: model.ErrTruncatedBox, Offset: int64(rb.info.Offset), Message: "mfhd truncated"}
			}
			st.seq = seq
			st.hasMfhd = true
			st.seqOffset = int64(rb.info.Offset)
		case "traf":
			st.trafSeen = true
		case "tfhd":
			// b 是 FullBox 字段区：verflags(4)+track_ID(4)，track_ID 在偏移 4。
			if len(rb.payload) < 8 {
				return &model.MediaError{Category: model.ErrTruncatedBox, Offset: int64(rb.info.Offset), Message: "tfhd truncated"}
			}
			st.trackID = binary.BigEndian.Uint32(rb.payload[4:8])
			if tm, ok := tracks[st.trackID]; ok {
				st.timescale = tm.Timescale
			}
		case "tfdt":
			v, ok := parseTfdt(rb.payload)
			if !ok {
				return &model.MediaError{Category: model.ErrTruncatedBox, Offset: int64(rb.info.Offset), Message: "tfdt truncated"}
			}
			st.tfdt = v
			st.hasTfdt = true
		case "trun":
			ti, ok := parseTrunHeader(rb.payload)
			if !ok {
				return &model.MediaError{Category: model.ErrTruncatedBox, Offset: int64(rb.info.Offset), Message: "trun truncated"}
			}
			st.truns = append(st.truns, trunHold{
				info:    ti,
				baseOff: int64(rb.info.Offset + rb.info.HeaderSize),
			})
		case "mdat":
			mdatPayload = rb.payload
			mdatOffset = int64(rb.info.Offset + rb.info.HeaderSize)
		}
		return nil
	})
	if err != nil {
		return model.Fragment{}, err
	}
	if !st.hasMfhd {
		return model.Fragment{}, &model.MediaError{
			Category: model.ErrMissingMFHD,
			Message:  "fragment contains no mfhd box",
			Offset:   0,
			TrackID:  st.trackID,
		}
	}
	if !st.trafSeen {
		return model.Fragment{}, &model.MediaError{Category: model.ErrTruncatedBox, Message: "moof without traf", TrackID: st.trackID}
	}
	if st.timescale == 0 {
		// 回退：分片自带 moof 无时基时禁止猜测，交给上层按错误类别处理。
		return model.Fragment{}, &model.MediaError{
			Category: model.ErrUnknownCodec,
			Message:  "fragment track timescale unknown",
			TrackID:  st.trackID,
		}
	}
	frag := model.Fragment{
		Sequence:            st.seq,
		Timescale:           st.timescale,
		BaseMediaDecodeTime: st.tfdt,
		Boxes:               st.boxes,
		RawSize:             int64(len(data)),
	}
	// 依据 trun 切分 mdat 样本；夹具每分片一个 trun，内核仍按多 trun 累加处理。
	cursor := mdatOffset
	baseTicks := st.tfdt
	for _, th := range st.truns {
		ti := th.info
		for i := uint32(0); i < ti.sampleCount; i++ {
			entry := th.baseOff + int64(ti.entryStarts) + int64(i)*int64(ti.entryStride)
			if entry+int64(ti.entryStride) > int64(len(data)) {
				return model.Fragment{}, &model.MediaError{Category: model.ErrTruncatedBox, Message: "trun entry out of range"}
			}
			var dur, size uint32
			field := int(entry)
			if ti.hasDuration {
				dur = binary.BigEndian.Uint32(data[field : field+4])
				field += 4
			}
			if ti.hasSize {
				size = binary.BigEndian.Uint32(data[field : field+4])
			}
			start := cursor - mdatOffset
			if uint64(start)+uint64(size) > uint64(len(mdatPayload)) {
				return model.Fragment{}, &model.MediaError{Category: model.ErrTruncatedBox, Message: "sample exceeds mdat payload"}
			}
			payload := make([]byte, size)
			copy(payload, mdatPayload[start:start+int64(size)])
			frag.Samples = append(frag.Samples, model.Sample{
				TrackID: st.trackID, Data: payload, Duration: int64(dur),
			})
			startTime := model.FromTimescale(baseTicks, st.timescale)
			endTime := model.FromTimescale(baseTicks+int64(dur), st.timescale)
			frag.Intervals = append(frag.Intervals, model.Interval{
				Start:       startTime,
				End:         endTime,
				FragmentSeq: st.seq,
				ByteOffset:  cursor,
				ByteLength:  int64(size),
			})
			baseTicks += int64(dur)
			cursor += int64(size)
		}
	}
	frag.ContentSHA = contentHash(data[:min(len(data), int(st.moofEnd))], mdatPayload)
	return frag, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
