package segment

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/abema/go-mp4"

	"mediatime/internal/clock"
)

// boxHeader 是解析出的顶层盒定位信息。
type boxHeader struct {
	typ    string // 盒四字符类型
	offset int64  // 盒起点（含 8/16 字节头）
	data   int64  // 负载起点
	size   int64  // 整盒大小
}

// parseTopLevelBoxes 顺序扫描顶层盒，任何声明越过输入边界即报截断/溢出，
// 绝不返回部分盒。
func parseTopLevelBoxes(data []byte) (map[string]boxHeader, error) {
	boxes := map[string]boxHeader{}
	pos := int64(0)
	for pos < int64(len(data)) {
		hdr, next, err := readBoxHeader(data, pos)
		if err != nil {
			return nil, err
		}
		boxes[hdr.typ] = hdr
		pos = next
	}
	return boxes, nil
}

func readBoxHeader(data []byte, pos int64) (boxHeader, int64, error) {
	if int64(len(data))-pos < 8 {
		return boxHeader{}, 0, ErrTruncatedBox
	}
	size32 := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
	typ := string(data[pos+4 : pos+8])
	headerSize := int64(8)
	size := size32
	switch size32 {
	case 1: // 64 位 largesize
		if int64(len(data))-pos < 16 {
			return boxHeader{}, 0, ErrTruncatedBox
		}
		size = int64(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
		headerSize = 16
	case 0: // 盒延伸到文件末尾
		size = int64(len(data)) - pos
	}
	if size < headerSize {
		return boxHeader{}, 0, fmt.Errorf("%w: %s size %d", ErrTruncatedBox, typ, size)
	}
	end := pos + size
	if end > int64(len(data)) || end < pos {
		return boxHeader{}, 0, fmt.Errorf("%w: %s ends at %d beyond %d", ErrBoxOverflow, typ, end, len(data))
	}
	return boxHeader{typ: typ, offset: pos, data: pos + headerSize, size: size}, end, nil
}

// ExtractFragment 从 fMP4 分片字节提取样本时间戳与文件偏移。
//
// 结构要求：styp? moof(traf(tfhd,tfdt,trun)) mdat。截断或越界直接返回
// 错误类别，调用方不会拿到半有效结果。
func ExtractFragment(sequence int, uri string, data []byte) (*ExtractedFragment, error) {
	top, err := parseTopLevelBoxes(data)
	if err != nil {
		return nil, &FragmentError{Sequence: sequence, Op: "parse boxes", Err: err}
	}
	moof, ok := top["moof"]
	if !ok {
		return nil, &FragmentError{Sequence: sequence, Op: "locate moof", Err: ErrNoMoof}
	}
	mdat, ok := top["mdat"]
	if !ok {
		return nil, &FragmentError{Sequence: sequence, Op: "locate mdat", Err: ErrNoMdat}
	}

	r := bytes.NewReader(data)
	tfhd, tfdt, trun, err := readTrafBoxes(r)
	if err != nil {
		return nil, &FragmentError{Sequence: sequence, Op: "read traf", Err: err}
	}
	trackID := tfhd.TrackID
	baseDTS := tfdt.GetBaseMediaDecodeTime()

	// 数据基准偏移：default-base-is-moof 时为 moof 起点，否则按 tfhd 显式值。
	var baseDataOffset int64
	if tfhd.GetFlags()&mp4.TfhdDefaultBaseIsMoof != 0 {
		baseDataOffset = moof.offset
	} else {
		baseDataOffset = int64(tfhd.BaseDataOffset)
	}

	n := int(trun.SampleCount)
	if n == 0 {
		return nil, &FragmentError{Sequence: sequence, Op: "trun", Err: ErrEmptySegment}
	}
	samples := make([]ExtractedSample, 0, n)
	cursor := baseDataOffset + int64(trun.DataOffset)
	var dtsCursor uint64 = baseDTS
	for i := 0; i < n; i++ {
		e := trun.Entries[i]
		size := e.SampleSize
		if size == 0 {
			size = tfhd.DefaultSampleSize
		}
		if size == 0 || uint64(size) > MaxSampleSize {
			return nil, &FragmentError{Sequence: sequence, Op: "sample size", Err: ErrBoxOverflow}
		}
		offset := cursor
		end := offset + int64(size)
		if offset < 0 || end > int64(len(data)) || end < offset {
			return nil, &FragmentError{Sequence: sequence, Op: "sample bounds", Err: ErrBoxOverflow}
		}
		dur := e.SampleDuration
		if dur == 0 {
			dur = tfhd.DefaultSampleDuration
		}
		cts := compositionOffset(e, trun.GetVersion())
		dts := dtsCursor
		pts := dts + uint64(int64(cts))
		sync := e.SampleFlags&0x10000 == 0 // sample_depends_on != 1（I 帧）
		samples = append(samples, ExtractedSample{
			SampleNumber: uint32(i + 1),
			TrackID:      trackID,
			RawDTS:       dts % clock.WrapMod,
			RawPTS:       pts % clock.WrapMod,
			IsSync:       sync,
			Offset:       offset,
			Size:         size,
		})
		dtsCursor += uint64(dur)
		cursor = end
	}
	return &ExtractedFragment{
		Sequence:   sequence,
		URI:        uri,
		Samples:    samples,
		MoofOffset: moof.offset,
		MdatOffset: mdat.offset,
		FileSize:   int64(len(data)),
	}, nil
}

func compositionOffset(e mp4.TrunEntry, version byte) int32 {
	if version == 0 {
		// v0 字段是无符号，按规范语义在此转回有符号值参与有符号运算。
		return int32(e.SampleCompositionTimeOffsetV0)
	}
	return e.SampleCompositionTimeOffsetV1
}

// readTrafBoxes 用 go-mp4 解码 moof 内的 tfhd/tfdt/trun，
// 解码失败（含截断）原样返回错误。
func readTrafBoxes(r *bytes.Reader) (*mp4.Tfhd, *mp4.Tfdt, *mp4.Trun, error) {
	if _, err := r.Seek(0, 0); err != nil {
		return nil, nil, nil, err
	}
	var (
		tfhdOut *mp4.Tfhd
		tfdtOut *mp4.Tfdt
		trunOut *mp4.Trun
	)
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type.String() {
		case "tfhd", "tfdt", "trun":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			switch b := box.(type) {
			case *mp4.Tfhd:
				tfhdOut = b
			case *mp4.Tfdt:
				tfdtOut = b
			case *mp4.Trun:
				trunOut = b
			}
		}
		return h.Expand()
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if tfhdOut == nil || tfdtOut == nil || trunOut == nil {
		return nil, nil, nil, ErrTruncatedBox
	}
	return tfhdOut, tfdtOut, trunOut, nil
}
