package parse

import (
	"fmt"
	"io"

	"github.com/abema/go-mp4"
)

// RawBox 是容错遍历得到的盒节点：保留偏移/层级，净荷按需读取。
type RawBox struct {
	Info     *mp4.BoxInfo
	Parent   *RawBox
	Children []*RawBox
	Depth    int
}

// Type 返回盒类型四字符码。
func (b *RawBox) Type() string { return b.Info.Type.String() }

// Find 在子树中按路径（相对）查找第一个匹配盒，nil 表示不存在。
func (b *RawBox) Find(path ...string) *RawBox {
	if len(path) == 0 {
		return b
	}
	for _, c := range b.Children {
		if c.Type() == path[0] {
			if r := c.Find(path[1:]...); r != nil {
				return r
			}
		}
	}
	return nil
}

// All 收集子树中所有类型匹配的盒。
func (b *RawBox) All(boxType string) []*RawBox {
	var out []*RawBox
	var walk func(*RawBox)
	walk = func(x *RawBox) {
		if x.Type() == boxType {
			out = append(out, x)
		}
		for _, c := range x.Children {
			walk(c)
		}
	}
	walk(b)
	return out
}

// 已知容器盒集合；不在集合内的盒一律按叶子跳过净荷处理。
var containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"edts": true, "dinf": true, "udta": true, "meta": true, "wave": true,
	"mvex": true, "moof": true, "traf": true, "sinf": true, "schi": true,
	"iref": true, "ilst": true,
}

// Walk 对整个流做容错盒遍历。尾部截断时保留最后一个完整盒并记录边界；
// 坏盒不会导致后续可独立解析的片段被整体丢弃。
func Walk(r io.ReadSeeker, lim Limits) ([]*RawBox, *Report) {
	rep := &Report{}
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		rep.add(&Error{Level: LevelFatal, BoxType: "-", Offset: -1, Message: err.Error()})
		return nil, rep
	}
	if lim.MaxFileSize > 0 && size > lim.MaxFileSize {
		rep.add(&Error{Level: LevelFatal, BoxType: "-", Offset: 0,
			Message: fmt.Sprintf("file size %d exceeds limit %d", size, lim.MaxFileSize)})
		return nil, rep
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		rep.add(&Error{Level: LevelFatal, BoxType: "-", Message: err.Error()})
		return nil, rep
	}
	roots := walkLevel(r, 0, uint64(size), 0, nil, lim, rep)
	return roots, rep
}

func walkLevel(r io.ReadSeeker, base, end uint64, depth int,
	parent *RawBox, lim Limits, rep *Report) []*RawBox {
	var nodes []*RawBox
	pos := base
	for pos < end {
		if rep.BoxesSeen >= lim.MaxBoxes {
			rep.add(&Error{Level: LevelSkipped, BoxType: "-", Offset: int64(pos),
				Message: "box count limit reached"})
			return nodes
		}
		if _, err := r.Seek(int64(pos), io.SeekStart); err != nil {
			rep.add(&Error{Level: LevelFatal, Offset: int64(pos), Message: err.Error()})
			return nodes
		}
		bi, err := mp4.ReadBoxInfo(r)
		if err != nil {
			// 尾部截断：当前位置之后没有完整盒头，遍历在本层结束。
			rep.Truncated = true
			rep.Boundaries = append(rep.Boundaries, Boundary{
				Offset: int64(pos), Kind: "truncated", Level: LevelRecoverable,
				Message: "incomplete box header at tail"})
			return nodes
		}
		rep.BoxesSeen++
		if bi.Size < bi.HeaderSize || (bi.Offset+bi.Size) > end {
			// 盒长度越界：记录坏盒边界，本层停止（无法定位后续盒）。
			rep.Truncated = true
			rep.add(&Error{Level: LevelRecoverable, BoxType: bi.Type.String(),
				Offset: int64(bi.Offset),
				Message: fmt.Sprintf("box size %d out of range [end=%d]", bi.Size, end)})
			rep.Boundaries = append(rep.Boundaries, Boundary{
				Offset: int64(bi.Offset), BoxType: bi.Type.String(),
				Kind: "bad_box", Level: LevelRecoverable,
				Message:  "declared size exceeds container",
				Recovered: int64(bi.Offset)})
			return nodes
		}
		node := &RawBox{Info: bi, Parent: parent, Depth: depth}
		rep.BoxesParsed++
		if containers[bi.Type.String()] && depth < lim.MaxDepth {
			node.Children = walkLevel(r, bi.Offset+bi.HeaderSize,
				bi.Offset+bi.Size, depth+1, node, lim, rep)
		} else if containers[bi.Type.String()] && depth == lim.MaxDepth {
			rep.add(&Error{Level: LevelSkipped, BoxType: bi.Type.String(),
				Offset: int64(bi.Offset), Message: "max parse depth reached"})
			rep.Boundaries = append(rep.Boundaries, Boundary{
				Offset: int64(bi.Offset), BoxType: bi.Type.String(),
				Kind: "depth", Level: LevelSkipped})
		}
		nodes = append(nodes, node)
		pos = bi.Offset + bi.Size
	}
	return nodes
}

// ReadPayload 将盒净荷读入内存，并强制执行单盒大小限制。
func ReadPayload(r io.ReadSeeker, b *RawBox, lim Limits) ([]byte, error) {
	n := int64(b.Info.Size - b.Info.HeaderSize)
	if n > lim.MaxPayload {
		return nil, &Error{Level: LevelSkipped, BoxType: b.Type(),
			Offset: int64(b.Info.Offset),
			Message: fmt.Sprintf("payload %d exceeds limit %d", n, lim.MaxPayload)}
	}
	if _, err := r.Seek(int64(b.Info.Offset+b.Info.HeaderSize), io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, &Error{Level: LevelRecoverable, BoxType: b.Type(),
			Offset: int64(b.Info.Offset), Message: "truncated payload: " + err.Error()}
	}
	return buf, nil
}

// Unmarshal 将指定盒净荷反序列化到 go-mp4 的语义结构。
func Unmarshal(r io.ReadSeeker, b *RawBox, dst mp4.IBox) error {
	if _, err := b.Info.SeekToPayload(r); err != nil {
		return err
	}
	_, err := mp4.Unmarshal(r, b.Info.Size-b.Info.HeaderSize, dst, b.Info.Context)
	return err
}
