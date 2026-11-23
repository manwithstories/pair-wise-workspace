// Package ebml 提供流式 EBML/WebM 元素读取。
// 支持未知长度元素，截断时报告字节偏移与可恢复边界，
// 并拒绝超过上限的元素长度。
package ebml

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// 常用元素 ID。
const (
	IDEBML          uint32 = 0x1A45DFA3
	IDSegment       uint32 = 0x18538067
	IDInfo          uint32 = 0x1549A966
	IDTimecodeScale uint32 = 0x2AD7B1
	IDTracks        uint32 = 0x1654AE6B
	IDTrackEntry    uint32 = 0x00AE
	IDTrackNumber   uint32 = 0x00D7
	IDTrackType     uint32 = 0x0083
	IDCodecID       uint32 = 0x0086
	IDCluster       uint32 = 0x1F43B675
	IDTimecode      uint32 = 0x00E7
	IDSimpleBlock   uint32 = 0x00A3
	IDBlockGroup    uint32 = 0x00A0
	IDBlock         uint32 = 0x00A1
)

// UnknownSize 表示元素声明了未知长度。
const UnknownSize int64 = -1

// MaxElementSize 是单个元素数据区的字节上限，超出即拒绝。
const MaxElementSize = 64 << 20

// ErrInvalidVINT 表示变长整数起始字节非法。
var ErrInvalidVINT = errors.New("ebml: invalid variable-length integer")

// Element 是一个已解析的元素头。
type Element struct {
	ID         uint32
	Size       int64 // UnknownSize 表示未知长度
	Offset     int64 // 元素 ID 的字节偏移
	DataOffset int64 // 数据区的字节偏移
}

// Unknown 报告元素是否声明未知长度。
func (e Element) Unknown() bool { return e.Size == UnknownSize }

// TruncatedError 描述输入在元素中途被截断。
type TruncatedError struct {
	Offset int64  // 截断发生时的字节偏移
	ID     uint32 // 被截断的元素
	Want   int64  // 期望读取的字节数
	Got    int64  // 实际读到的字节数
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf("ebml: element 0x%X truncated at byte %d: want %d bytes, got %d",
		e.ID, e.Offset, e.Want, e.Got)
}

// TooLargeError 表示元素声明长度超过上限。
type TooLargeError struct {
	Offset int64
	ID     uint32
	Size   int64
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("ebml: element 0x%X at byte %d declares %d bytes, over limit %d",
		e.ID, e.Offset, e.Size, MaxElementSize)
}

// Reader 在流上顺序读取 EBML 元素并跟踪字节偏移。
type Reader struct {
	r    *bufio.Reader
	off  int64
	back *Element
}

// NewReader 包装一个 io.Reader。
func NewReader(r io.Reader) *Reader {
	if br, ok := r.(*bufio.Reader); ok {
		return &Reader{r: br}
	}
	return &Reader{r: bufio.NewReader(r)}
}

// Offset 返回当前读取位置的字节偏移。
func (r *Reader) Offset() int64 { return r.off }

func (r *Reader) byte() (byte, error) {
	b, err := r.r.ReadByte()
	if err != nil {
		return 0, err
	}
	r.off++
	return b, nil
}

// readVINT 读取 EBML 变长整数；keepMarker 保留长度标记位（用于元素 ID）。
func (r *Reader) readVINT(keepMarker bool) (uint64, int, error) {
	b0, err := r.byte()
	if err != nil {
		return 0, 0, err
	}
	if b0 == 0 {
		return 0, 0, ErrInvalidVINT
	}
	width := 1
	mask := byte(0x80)
	for b0&mask == 0 {
		width++
		mask >>= 1
	}
	val := uint64(b0)
	if !keepMarker {
		val &^= uint64(mask)
	}
	for i := 1; i < width; i++ {
		b, err := r.byte()
		if err != nil {
			return 0, 0, err
		}
		val = val<<8 | uint64(b)
	}
	return val, width, nil
}

// ReadElement 读取下一个元素头；流在元素边界干净结束时返回 io.EOF。
func (r *Reader) ReadElement() (Element, error) {
	if r.back != nil {
		el := *r.back
		r.back = nil
		return el, nil
	}
	start := r.off
	id, _, err := r.readVINT(true)
	if err != nil {
		if err == io.EOF && r.off > start {
			return Element{}, &TruncatedError{Offset: r.off, Want: 1, Got: 0}
		}
		return Element{}, err
	}
	size, width, err := r.readVINT(false)
	if err != nil {
		return Element{}, &TruncatedError{Offset: r.off, ID: uint32(id), Want: 1, Got: 0}
	}
	el := Element{ID: uint32(id), Size: int64(size), Offset: start, DataOffset: r.off}
	if size == 1<<(7*width)-1 {
		el.Size = UnknownSize
	}
	if el.Size > MaxElementSize {
		return Element{}, &TooLargeError{Offset: start, ID: el.ID, Size: el.Size}
	}
	return el, nil
}

// PushBack 推回一个尚未消费数据区的元素头，供上层重新判定层级。
func (r *Reader) PushBack(el Element) {
	if r.off != el.DataOffset {
		panic("ebml: pushback after data consumed")
	}
	r.back = &el
}

// ReadData 读取元素整个数据区；截断时返回 *TruncatedError。
func (r *Reader) ReadData(el Element) ([]byte, error) {
	if el.Unknown() {
		return nil, errors.New("ebml: cannot read data of unknown-size element")
	}
	buf := make([]byte, el.Size)
	n, err := io.ReadFull(r.r, buf)
	r.off += int64(n)
	if err != nil {
		return nil, &TruncatedError{Offset: el.DataOffset + int64(n), ID: el.ID, Want: el.Size, Got: int64(n)}
	}
	return buf, nil
}

// Skip 跳过元素数据区。
func (r *Reader) Skip(el Element) error {
	if el.Unknown() {
		return errors.New("ebml: cannot skip unknown-size element")
	}
	n, err := io.CopyN(io.Discard, r.r, el.Size)
	r.off += n
	if err != nil {
		return &TruncatedError{Offset: el.DataOffset + n, ID: el.ID, Want: el.Size, Got: n}
	}
	return nil
}
