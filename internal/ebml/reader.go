package ebml

import (
	"bytes"
	"io"

	"bcastqc/internal/media"
)

// Matroska/EBML 元素 ID（仅列本项目用到的子集）。
const (
	IDEBML            uint32 = 0x1A45DFA3
	IDEBMLVersion     uint32 = 0x4286
	IDDocType         uint32 = 0x4282
	IDSegment         uint32 = 0x18538067
	IDSeekHead        uint32 = 0x114D9B74
	IDInfo            uint32 = 0x1549A966
	IDTimecodeScale   uint32 = 0x2AD7B1
	IDDuration        uint32 = 0x4489
	IDTracks          uint32 = 0x1654AE6B
	IDTrackEntry      uint32 = 0xAE
	IDTrackNumber     uint32 = 0xD7
	IDTrackType       uint32 = 0x83
	IDDefaultDuration uint32 = 0x23E383
	IDCluster         uint32 = 0x1F43B675
	IDTimecode        uint32 = 0xE7
	IDSimpleBlock     uint32 = 0xA3
	IDBlock           uint32 = 0xA1
)

// UnknownSize 是 EBML 保留的“未知长度”全 1 模式。
const UnknownSize int64 = -1

// Element 描述一个已读取头部的 EBML 元素。
type Element struct {
	ID     uint32
	Size   int64 // UnknownSize 表示未知长度
	Start  int64 // 元素 ID 首字节的绝对偏移
	HdrEnd int64 // 数据区起始绝对偏移
	End    int64 // 数据区结束绝对偏移；未知长度为 -1
}

// Reader 是带绝对偏移记账的顺序 EBML 读取器。
// 父子读取器共享同一底层 io.Reader，各自记录当前位置与硬边界。
type Reader struct {
	r        io.Reader
	pos      int64 // 已消费字节的绝对偏移
	fileEnd  int64 // 物理输入末尾（>=0），越此为截断
	parent   *Element
}

// NewReader 包装底层读取器；limit>=0 时作为父元素硬边界。
func NewReader(r io.Reader, limit int64) *Reader {
	var parent *Element
	if limit >= 0 {
		parent = &Element{End: limit}
	}
	return &Reader{r: r, fileEnd: limit, parent: parent}
}

// NewBytesReader 从字节切片构建顶层读取器，便于测试与独立校验。
func NewBytesReader(b []byte) *Reader {
	return NewReader(bytes.NewReader(b), int64(len(b)))
}

// Offset 返回已消费字节的绝对偏移。
func (r *Reader) Offset() int64 { return r.pos }

func (r *Reader) truncErr(off int64, detail string) error {
	return media.NewError(media.StageParse, media.KindTruncated, detail).WithOffset(off)
}

// read 受父边界保护地填充 buf；EOF 截断时报告首个缺失字节偏移。
func (r *Reader) read(buf []byte) error {
	off := r.pos
	want := int64(len(buf))
	if r.parent != nil && r.parent.End >= 0 && off+want > r.parent.End {
		// 落在物理文件内但越过父元素声明边界：结构性越界。
		if r.fileEnd < 0 || off+want <= r.fileEnd {
			return media.NewError(media.StageParse, media.KindBoundary,
				"读取越过父级边界").WithOffset(r.parent.End)
		}
	}
	if _, err := io.ReadFull(r.r, buf); err != nil {
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return r.truncErr(off, "元素被截断，期望 "+itoa(len(buf))+" 字节")
		}
		return media.NewError(media.StageParse, media.KindIO, "底层读取失败").Wrap(err)
	}
	r.pos += want
	return nil
}

// ReadVInt 读取一个 EBML 变长无符号整数（元素 ID 或尺寸）。
// asID=true 时保留长度标记位（用于还原元素 ID）。
func (r *Reader) ReadVInt(asID bool) (value uint64, width int, err error) {
	off := r.pos
	var head [1]byte
	if e := r.read(head[:]); e != nil {
		return 0, 0, e
	}
	b := head[0]
	if b == 0 {
		return 0, 0, media.NewError(media.StageParse, media.KindBoundary,
			"VINT 首字节为 0x00").WithOffset(off)
	}
	width = 1
	for width <= 8 && b&(0x80>>(width-1)) == 0 {
		width++
	}
	if width > 8 {
		return 0, 0, media.NewError(media.StageParse, media.KindBoundary,
			"VINT 长度超过 8 字节").WithOffset(off)
	}
	rest := make([]byte, width-1)
	if e := r.read(rest); e != nil {
		return 0, 0, e
	}
	if asID {
		value = uint64(b)
	} else {
		value = uint64(b) & (0xFF >> width)
	}
	for _, x := range rest {
		value = (value << 8) | uint64(x)
	}
	allOnes := (uint64(1) << (7 * width)) - 1
	unknown := !asID && value == allOnes
	if unknown {
		// 宽度 0 作为“未知长度全 1 模式”的内部标记。
		return value, 0, nil
	}
	return value, width, nil
}

// Next 读取下一个兄弟元素的头部；父元素正常读完返回 io.EOF。
func (r *Reader) Next() (*Element, error) {
	if r.parent != nil && r.parent.End >= 0 && r.pos >= r.parent.End {
		return nil, io.EOF
	}
	start := r.pos
	id, _, err := r.ReadVInt(true)
	if err != nil {
		return nil, err
	}
	sizeRaw, sizeLen, err := r.ReadVInt(false)
	if err != nil {
		return nil, err
	}
	hdrEnd := r.pos
	el := &Element{ID: uint32(id), Start: start, HdrEnd: hdrEnd}
	if sizeLen == 0 {
		el.Size = UnknownSize
		el.End = -1
	} else {
		el.Size = int64(sizeRaw)
		el.End = hdrEnd + el.Size
		if r.parent != nil && r.parent.End >= 0 && el.End > r.parent.End {
			// 越过物理文件末尾：截断，报首个缺失偏移；否则为结构越界。
			if r.fileEnd >= 0 && el.End > r.fileEnd {
				return nil, r.truncErr(r.fileEnd,
					"元素声明尺寸超出物理输入末尾")
			}
			return nil, media.NewError(media.StageParse, media.KindBoundary,
				"元素声明尺寸越过父级边界").WithOffset(r.parent.End)
		}
	}
	return el, nil
}

// Child 返回限定在当前元素数据区内的子读取器（支持未知长度父元素）。
// 调用方在子读取器上消费完毕后应调用 Drain 对齐到元素末尾。
func (r *Reader) Child(el *Element) *Reader {
	limit := el.End // 未知长度时为 -1，子级继承当前父边界
	if limit < 0 && r.parent != nil {
		limit = r.parent.End
	}
	return &Reader{
		r:       r.r,
		pos:     el.HdrEnd,
		fileEnd: r.fileEnd,
		parent:  &Element{Start: el.HdrEnd, HdrEnd: el.HdrEnd, End: limit},
	}
}

// Drain 消费元素数据区剩余字节；未知长度元素为空操作。
func (r *Reader) Drain(el *Element) error {
	if el.Size == UnknownSize {
		return nil
	}
	off := r.pos
	need := el.End - off
	if need <= 0 {
		return nil
	}
	if r.parent != nil && r.parent.End >= 0 && el.End > r.parent.End {
		return media.NewError(media.StageParse, media.KindBoundary,
			"数据区越过父级边界").WithOffset(el.End)
	}
	if _, err := io.CopyN(io.Discard, r.r, need); err != nil {
		return r.truncErr(off, "数据区不完整")
	}
	r.pos += need
	return nil
}

// ReadData 读出整个（小）叶子元素的数据区并对齐位置。
func (r *Reader) ReadData(el *Element) ([]byte, error) {
	if el.Size == UnknownSize {
		return nil, media.NewError(media.StageParse, media.KindBoundary,
			"叶子元素不允许未知长度").WithOffset(el.HdrEnd)
	}
	buf := make([]byte, el.Size)
	if err := r.read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
