// Package segment 在严格内存边界内读取本地分片，从 fMP4（moof/traf/trun）
// 样本表提取样本，并重建可独立核验的随机访问索引。
//
// 安全边界：单任务最多 MaxSegmentBytes（64 MiB）；任何声明越过文件末尾的
// 盒或样本都返回结构化错误，截断盒不会产出部分有效索引。
package segment

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"mediatime/internal/clock"
)

// 硬性限制。
const (
	// MaxSegmentBytes 单个分片任务最多读取 64 MiB。
	MaxSegmentBytes = 64 << 20
	// MaxSampleSize 单样本声明大小上限，防御整数溢出与巨型盒。
	MaxSampleSize = 16 << 20
)

// 错误类别（携带片段编号定位信息，由上层包装）。
var (
	// ErrTruncatedBox 盒结构在声明长度前结束或越界。
	ErrTruncatedBox = errors.New("segment: truncated_box")
	// ErrBoxOverflow 盒/样本声明偏移越过输入边界。
	ErrBoxOverflow = errors.New("segment: box_overflow")
	// ErrSegmentTooLarge 分片超过 64 MiB 任务上限。
	ErrSegmentTooLarge = errors.New("segment: segment too large")
	// ErrNoMoof 分片缺少 moof 容器，无法提取样本。
	ErrNoMoof = errors.New("segment: missing moof box")
	// ErrNoMdat 分片缺少 mdat 负载。
	ErrNoMdat = errors.New("segment: missing mdat box")
	// ErrEmptySegment 分片不含任何样本。
	ErrEmptySegment = errors.New("segment: no samples")
)

// FragmentError 把错误类别与片段序号绑定，保证跨模块故障可定位。
type FragmentError struct {
	Sequence int
	Op       string
	Err      error
}

func (e *FragmentError) Error() string {
	return fmt.Sprintf("segment: fragment %d %s: %v", e.Sequence, e.Op, e.Err)
}

// Unwrap 支持 errors.Is/As 穿透到原始类别。
func (e *FragmentError) Unwrap() error { return e.Err }

// ReadAll 从 r 读取至多 MaxSegmentBytes 字节；超出限制返回 ErrSegmentTooLarge。
func ReadAll(r *bytes.Reader) ([]byte, error) {
	size := r.Len()
	if size > MaxSegmentBytes {
		return nil, ErrSegmentTooLarge
	}
	out := make([]byte, size)
	n, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return out[:len(n)], nil
}

// ExtractedSample 是从一个分片提取出的样本描述，时间戳为原始 33 位值，
// 后续交给 clock 修复器展开。
type ExtractedSample struct {
	SampleNumber uint32
	TrackID      uint32
	RawDTS       uint64
	RawPTS       uint64
	IsSync       bool
	// Offset 是样本负载相对分片文件起始的绝对字节偏移。
	Offset int64
	Size   uint32
}

// ExtractedFragment 是一个分片的提取结果。
type ExtractedFragment struct {
	Sequence int
	URI      string
	Samples  []ExtractedSample
	// MoofOffset/ MdatOffset 记录盒级偏移，供索引器核验边界。
	MoofOffset int64
	MdatOffset int64
	FileSize   int64
}

// ToClockSamples 把提取结果转换为 clock.Sample（修复前原始态）。
func (f *ExtractedFragment) ToClockSamples() []clock.Sample {
	out := make([]clock.Sample, 0, len(f.Samples))
	for _, s := range f.Samples {
		out = append(out, clock.Sample{
			Sequence:     f.Sequence,
			TrackID:      s.TrackID,
			SampleNumber: s.SampleNumber,
			RawDTS:       s.RawDTS,
			RawPTS:       s.RawPTS,
			IsSync:       s.IsSync,
		})
	}
	return out
}
