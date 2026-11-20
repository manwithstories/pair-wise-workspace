package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
)

// Scan 读取完整裸流并按块提交。reader 返回的字节即磁盘上的原始内容，
// 块校验和覆盖原始字节，使恢复时能逐块验证输入摘要。
func (s *Scanner) Scan(r io.Reader) (*ScanResult, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	size := int64(len(raw))
	s.pos = 0
	s.blockStart = 0
	s.blockHash = sha256.New()

	units, splitIssues, size, err := splitUnits(newByteReader(raw))
	if err != nil {
		return nil, err
	}
	for i := range splitIssues {
		splitIssues[i].TrackID = s.trackID
		splitIssues[i].Block = s.blockIndex
		s.issues = append(s.issues, splitIssues[i])
	}
	// 最后一个单元之后若存在残余字节，属于被截断的不完整单元。
	if len(units) > 0 && units[len(units)-1].end < size {
		last := units[len(units)-1].end
		s.issues = append(s.issues, Issue{
			TrackID: s.trackID,
			Offset:  last,
			End:     size,
			Block:   s.blockIndex,
			Kind:    IssueUnterminated,
			Message: "流尾部存在不完整语法单元",
		})
	} else if len(units) == 0 && size > 0 {
		s.issues = append(s.issues, Issue{
			TrackID: s.trackID,
			Offset:  0,
			End:     size,
			Block:   s.blockIndex,
			Kind:    IssueUnterminated,
			Message: "流起始处缺少起始码",
		})
	}
	// 起始码之间若存在不属于任何单元的孤立字节，按截断报告。
	if len(units) > 0 && units[0].start > 0 {
		s.issues = append(s.issues, Issue{
			TrackID: s.trackID,
			Offset:  0,
			End:     units[0].start,
			Block:   s.blockIndex,
			Kind:    IssueUnterminated,
			Message: "首个起始码前存在孤立字节",
		})
	}

	auRanges := s.groupAUs(raw, units)
	for idx, ar := range auRanges {
		s.processAU(raw, units, idx, ar)
	}
	// 尾部残余（如不完整单元）纳入最后一块。
	if s.pos < size {
		_, _ = s.blockHash.Write(raw[s.pos:])
		s.pos = size
	}
	if len(s.blocks) == 0 || s.blockAU > 0 || size == 0 {
		s.finishBlock(size)
	}
	return &ScanResult{
		Codec:   s.codec,
		TrackID: s.trackID,
		AUs:     s.aus,
		Issues:  s.issues,
		Blocks:  s.blocks,
		Size:    size,
	}, nil
}

// auRange 是一个访问单元覆盖的单元索引区间 [lo, hi)。
type auRange struct {
	lo, hi int
}

// groupAUs 把语法单元聚合为 AU。H.264/H.265 以 VCL NAL 的边界切分，
// 参数集 NAL 附着到下一个 VCL AU；AV1 每个帧 OBU 即一个 AU，
// 非帧 OBU（序列/元数据/填充）附着到下一个帧 AU。
func (s *Scanner) groupAUs(b []byte, units []unit) []auRange {
	var ranges []auRange
	lo := 0
	flush := func(hi int) {
		if hi > lo {
			ranges = append(ranges, auRange{lo, hi})
		}
		lo = hi
	}
	switch s.codec {
	case H264, H265:
		// 记录每个 VCL NAL 的单元下标。AU 边界 = 相邻 VCL 之间；
		// 第一个 VCL 之前的前导参数集（SPS/PPS/VPS）并入首 AU，
		// 两个 VCL 之间的非 VCL 并入后一个 AU。
		var vclIdx []int
		for i, u := range units {
			if isVCL(s.codec, b[u.data:u.end]) {
				vclIdx = append(vclIdx, i)
			}
		}
		if len(vclIdx) == 0 {
			flush(len(units))
		} else {
			start := 0
			for k := 1; k < len(vclIdx); k++ {
				flush(vclIdx[k]) // [start, vclIdx[k]) 是上一帧 AU
				start = vclIdx[k]
			}
			_ = start
			flush(len(units))
		}
	case AV1:
		seenFrame := false
		for i, u := range units {
			if isAV1FrameOBU(b[u.data:u.end]) {
				if seenFrame {
					flush(i)
				}
				seenFrame = true
			}
		}
		flush(len(units))
	}
	return ranges
}

func (s *Scanner) processAU(b []byte, units []unit, idx int, ar auRange) {
	start := units[ar.lo].start
	end := units[ar.hi-1].end
	keyframe, kind, msg, errOff := s.classify(b, units, ar)
	au := AccessUnit{
		Index:    idx,
		Offset:   start,
		End:      end,
		Block:    s.blockIndex,
		Keyframe: keyframe,
		HasError: kind != "",
	}
	if s.pos < end {
		_, _ = s.blockHash.Write(b[s.pos:end])
		s.pos = end
	}
	s.aus = append(s.aus, au)
	s.blockAU++
	if kind != "" {
		if errOff < 0 {
			errOff = start
		}
		s.issues = append(s.issues, Issue{
			TrackID: s.trackID,
			Offset:  errOff,
			End:     end,
			Block:   s.blockIndex,
			Kind:    kind,
			Message: msg,
		})
	}
	if s.blockAU >= s.blockFrames {
		s.finishBlock(end)
	}
}

func (s *Scanner) finishBlock(end int64) {
	sum := s.blockHash.Sum(nil)
	s.blocks = append(s.blocks, BlockRecord{
		Number: s.blockIndex,
		Start:  s.blockStart,
		End:    end,
		SHA256: hex.EncodeToString(sum),
	})
	s.blockIndex++
	s.blockStart = end
	s.blockAU = 0
	s.blockHash = sha256.New()
}
