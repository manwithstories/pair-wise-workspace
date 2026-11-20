// Package stream 增量解析 H.264/H.265/AV1 裸码流，识别访问单元（AU）与
// 随机访问帧，并记录每个 AU 的源字节区间、所属块号与结构化错误。
package stream

import (
	"crypto/sha256"
	"errors"
	"hash"
	"io"
)

// Codec 标识支持的编码格式。
type Codec string

// 支持的三种编码。
const (
	H264 Codec = "h264"
	H265 Codec = "h265"
	AV1  Codec = "av1"
)

// ParseCodec 把命令行字符串解析为 Codec。
func ParseCodec(s string) (Codec, error) {
	switch Codec(s) {
	case H264:
		return H264, nil
	case H265, "hevc":
		return H265, nil
	case AV1:
		return AV1, nil
	default:
		return "", errors.New("unsupported codec: " + s)
	}
}

// IssueKind 是错误类别枚举，供清单错误摘要与退出码判定使用。
type IssueKind string

// 扫描阶段可产生的错误类别。
const (
	IssueTruncated     IssueKind = "truncated_unit"      // 单元被截断
	IssueForbiddenNAL  IssueKind = "forbidden_nal_bit"   // NAL forbidden_zero_bit 被置 1
	IssueIllegalOBU    IssueKind = "illegal_obu_header"  // OBU 头非法（禁用位/保留位）
	IssueEmptyUnit     IssueKind = "empty_unit"          // 零长度单元
	IssueUnterminated  IssueKind = "unterminated_stream" // 流尾部存在不完整单元
)

// Issue 是一条结构化错误记录，携带轨道、字节偏移、块号与类别。
type Issue struct {
	TrackID  int       `json:"track_id"`
	Offset   int64     `json:"offset_bytes"`
	End      int64     `json:"end_bytes"`
	Block    int       `json:"block"`
	Kind     IssueKind `json:"kind"`
	Message  string    `json:"message"`
}

// AccessUnit 是一个访问单元的扫描记录。
type AccessUnit struct {
	Index    int   `json:"index"`
	Offset   int64 `json:"offset_bytes"`
	End      int64 `json:"end_bytes"`
	Block    int   `json:"block"`
	Keyframe bool  `json:"keyframe"`
	HasError bool  `json:"has_error"`
}

// BlockRecord 记录一个扫描块的字节区间与内容校验和，供检查点恢复复用。
type BlockRecord struct {
	Number int    `json:"number"`
	Start  int64  `json:"start_bytes"`
	End    int64  `json:"end_bytes"`
	SHA256 string `json:"sha256"`
}

// ScanResult 是整条流的扫描结果。
type ScanResult struct {
	Codec    Codec
	TrackID  int
	AUs      []AccessUnit
	Issues   []Issue
	Blocks   []BlockRecord
	Size     int64
}

// unit 是 Annex-B / 低开销 OBU 流中的一个语法单元（NAL 或 OBU）。
type unit struct {
	start int64 // 起始码首字节偏移
	data  int64 // 净荷首字节偏移（起始码之后）
	end   int64 // 单元结束偏移（不含）
}

// splitUnits 扫描 Annex-B 起始码（00 00 01 / 00 00 00 01）。AV1 低开销
// 码流格式复用相同的成帧方式。返回的单元按偏移升序排列。
func splitUnits(r io.Reader) ([]unit, []Issue, int64, error) {
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, 0, err
	}
	n := int64(len(buf))
	var units []unit
	var issues []Issue
	pos, scLen, ok := findStartCode(buf, 0)
	if !ok {
		if n > 0 {
			issues = append(issues, Issue{
				Offset: 0, End: n, Block: -1,
				Kind: IssueUnterminated, Message: "流起始处缺少起始码",
			})
		}
		return units, issues, n, nil
	}
	for ok {
		payload := pos + scLen
		nextPos, nextLen, found := findStartCode(buf, payload)
		if found {
			units = append(units, unit{start: pos, data: payload, end: nextPos})
			pos, scLen = nextPos, nextLen
			continue
		}
		// 之后再无起始码：净荷即最后一个单元；任何多余残余由调用方诊断。
		units = append(units, unit{start: pos, data: payload, end: n})
		break
	}
	// Annex-B RBSP 受竞争防止保护，合法单元内部不可能出现裸 00 00；
	// 因此流尾部 1–3 个连续零字节是写入中断留下的半截起始码
	//（4 个零必含完整起始码，findStartCode 已先行处理）。
	if len(units) > 0 {
		z := int64(0)
		for z < 3 && n-z-1 >= 0 && buf[n-z-1] == 0 {
			z++
		}
		if z > 0 {
			last := &units[len(units)-1]
			cut := n - z
			if cut > last.data {
				last.end = cut
				issues = append(issues, Issue{
					Offset: cut, End: n, Block: -1,
					Kind: IssueUnterminated, Message: "流尾部存在不完整起始码",
				})
			}
		}
	}
	return units, issues, n, nil
}

// findStartCode 返回从 from 起第一个起始码的首字节偏移与其长度（3 或 4）。
func findStartCode(b []byte, from int64) (pos, length int64, ok bool) {
	for i := from; i+2 < int64(len(b)); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if i > from && b[i-1] == 0 {
				return i - 1, 4, true
			}
			return i, 3, true
		}
	}
	return 0, 0, false
}

// Scanner 按可配置块大小增量扫描单条裸流。
type Scanner struct {
	codec      Codec
	trackID    int
	blockFrames int
	aus        []AccessUnit
	issues     []Issue
	blocks     []BlockRecord
	blockStart int64
	blockAU    int
	blockHash  hash.Hash
	blockIndex int
	pos        int64
}

// NewScanner 创建扫描器。blockFrames 为每块期望帧数（按 AU 计数）。
func NewScanner(codec Codec, trackID, blockFrames int) *Scanner {
	return &Scanner{
		codec:       codec,
		trackID:     trackID,
		blockFrames: blockFrames,
		blockHash:   sha256.New(),
	}
}
