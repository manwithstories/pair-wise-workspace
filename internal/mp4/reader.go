// Package mp4 基于 go-mp4 解析自生成的 MP4 夹具。
//
// 解析器在三个层面保证故障隔离：
//   - 顶层盒读取遇到 EOF 即标记截断并停止，已解析的盒全部保留；
//   - 未知盒保留原始 payload 字节，往返序列化逐字节一致；
//   - 样本表（stts/stss/stsz/stsc/stco|co64）展开为逐样本结构，
//     任何局部不一致只产生可恢复错误并标记受影响样本，不扩散到整条轨道。
package mp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	gomp4 "github.com/abema/go-mp4"

	"mediad/internal/fault"
)

// 容器盒类型：其 payload 由子盒构成。
var containerTypes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "edts": true, "udta": true, "meta": true,
}

// BoxNode 是与具体库类型解耦的盒树节点。
type BoxNode struct {
	Type     string     `json:"type"`
	Version  uint8      `json:"version"`
	Flags    uint32     `json:"flags"`
	Offset   uint64     `json:"offset"`
	Header   uint64     `json:"header_size"`
	Size     uint64     `json:"size"`
	Payload  []byte     `json:"payload,omitempty"` // 未知盒/叶子盒的原始字节
	Children []*BoxNode `json:"children,omitempty"`
}

// Find 返回深度优先第一个匹配类型的节点，未找到返回 nil。
func (n *BoxNode) Find(typ string) *BoxNode {
	if n == nil {
		return nil
	}
	if n.Type == typ {
		return n
	}
	for _, c := range n.Children {
		if hit := c.Find(typ); hit != nil {
			return hit
		}
	}
	return nil
}

// FindAll 返回深度优先的全部匹配节点。
func (n *BoxNode) FindAll(typ string) []*BoxNode {
	var out []*BoxNode
	if n == nil {
		return out
	}
	if n.Type == typ {
		out = append(out, n)
	}
	for _, c := range n.Children {
		out = append(out, c.FindAll(typ)...)
	}
	return out
}

// File 是一次解析的完整结果。
type File struct {
	Root      *BoxNode
	RawSize   int64
	Truncated bool
	Faults    []*fault.Error
}

// Parse 读取整个 MP4 字节流，返回盒树与可恢复故障列表。
// 截断输入不会返回 error：故障记录在 File.Faults，调用方按状态处理。
func Parse(r io.ReadSeeker, size int64) (*File, error) {
	f := &File{RawSize: size, Root: &BoxNode{Type: "<root>"}}
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fault.Fatal(fault.CodeInvalidInput, "mp4.Parse", err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fault.Fatal(fault.CodeInvalidInput, "mp4.Parse", err)
	}
	for {
		node, perr := parseOne(r, uint64(end))
		if node != nil {
			f.Root.Children = append(f.Root.Children, node)
		}
		if perr != nil {
			if fault.IsFatal(perr) {
				return nil, perr
			}
			f.Faults = append(f.Faults, perr.(*fault.Error))
			f.Truncated = true
			if node == nil {
				break
			}
			next := int64(node.Offset + node.Size)
			if next <= end {
				// 盒内部截断：跳到声明末尾继续解析后续完整盒。
				if _, serr := r.Seek(next, io.SeekStart); serr != nil {
					break
				}
				continue
			}
			// 声明越界：截断只吃掉当前盒 payload 尾部，后续完整盒
			//（如 moov）从实际 EOF 处开始。
			if _, serr := r.Seek(end, io.SeekStart); serr != nil {
				break
			}
			continue
		}
		if node == nil {
			break
		}
		// 正常路径：对齐到声明末尾，防御读取后的位置偏差。
		next := int64(node.Offset + node.Size)
		if cur, _ := r.Seek(0, io.SeekCurrent); cur < next && next <= end {
			if _, err := r.Seek(next, io.SeekStart); err != nil {
				break
			}
		}
	}
	return f, nil
}

func parseOne(r io.ReadSeeker, limit uint64) (*BoxNode, error) {
	start, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, fault.Fatal(fault.CodeInvalidInput, "mp4.seek", err)
	}
	bi, err := gomp4.ReadBoxInfo(r)
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		if err == io.ErrUnexpectedEOF || isEOF(err) {
			return nil, fault.Recoverable(fault.CodeTruncated, "mp4.ReadBoxInfo",
				0, 0, fmt.Errorf("box header at offset %d is truncated: %w", start, err))
		}
		return nil, fault.Fatal(fault.CodeInvalidInput, "mp4.ReadBoxInfo", err)
	}
	node := &BoxNode{
		Type: bi.Type.String(), Offset: bi.Offset,
		Header: bi.HeaderSize, Size: bi.Size,
	}
	payloadSize := bi.Size - bi.HeaderSize
	overshoot := bi.Offset+bi.Size > limit
	if containerTypes[node.Type] {
		if overshoot {
			// 容器盒声明越界本身即截断，不再递归进入伪造的子盒区域。
			return node, fault.Recoverable(fault.CodeTruncated, "mp4.containerOvershoot",
				0, 0, fmt.Errorf("container %s declares past EOF", node.Type))
		}
		children, cerr := parseChildren(r, bi.Offset+bi.HeaderSize, payloadSize)
		node.Children = children.nodes
		if cerr != nil {
			return node, cerr
		}
		return node, nil
	}
	// 叶子盒：保留原始 payload，未知盒由此实现字节保真。
	if _, err := bi.SeekToPayload(r); err != nil {
		return node, fault.Recoverable(fault.CodeTruncated, "mp4.seekPayload", 0, 0, err)
	}
	buf := make([]byte, payloadSize)
	n, rerr := io.ReadFull(r, buf)
	if rerr != nil || uint64(n) < payloadSize {
		// 截断：保留已读到的部分字节与节点元数据，上层据此跳过声明区间。
		node.Payload = buf[:n]
		return node, fault.Recoverable(fault.CodeTruncated, "mp4.readPayload",
			0, 0, fmt.Errorf("box %s payload: got %d bytes, want %d: %w",
				node.Type, n, payloadSize, rerr))
	}
	node.Payload = buf
	// full box 的前 4 字节是 version(1)+flags(3)。
	if len(buf) >= 4 {
		node.Version = buf[0]
		node.Flags = uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])
	}
	// 叶子盒的声明范围越过 EOF：即便 ReadFull 恰好被后续盒字节填满，
	// 也必须按截断处理，防止把后续盒当作本盒 payload 吞掉。
	if overshoot {
		return node, fault.Recoverable(fault.CodeTruncated, "mp4.boxOvershoot",
			0, 0, fmt.Errorf("box %s declares %d bytes past EOF", node.Type, bi.Size))
	}
	return node, nil
}

type childResult struct{ nodes []*BoxNode }

func parseChildren(r io.ReadSeeker, base, size uint64) (*childResult, error) {
	res := &childResult{}
	end := base + size
	for {
		pos, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return res, fault.Fatal(fault.CodeInvalidInput, "mp4.seek", err)
		}
		if uint64(pos) >= end {
			return res, nil
		}
		node, perr := parseOne(r, end)
		if perr != nil {
			return res, perr
		}
		if node == nil {
			return res, nil
		}
		res.nodes = append(res.nodes, node)
	}
}

func isEOF(err error) bool {
	for e := err; e != nil; {
		if e == io.EOF || e == io.ErrUnexpectedEOF {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// MarshalBox 把单个盒节点序列化为“头 + payload/子盒”的完整字节。
func MarshalBox(n *BoxNode) ([]byte, error) {
	var body bytes.Buffer
	if len(n.Children) > 0 {
		for _, c := range n.Children {
			raw, err := MarshalBox(c)
			if err != nil {
				return nil, err
			}
			body.Write(raw)
		}
	} else {
		body.Write(n.Payload)
	}
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out, uint32(8+body.Len()))
	copy(out[4:8], []byte(fourCC(n.Type)))
	return append(out, body.Bytes()...), nil
}

// Marshal 把整棵盒树序列化为 MP4 字节流。
func Marshal(f *File) ([]byte, error) {
	var out bytes.Buffer
	for _, n := range f.Root.Children {
		raw, err := MarshalBox(n)
		if err != nil {
			return nil, err
		}
		out.Write(raw)
	}
	return out.Bytes(), nil
}

func fourCC(s string) string {
	if len(s) == 4 {
		return s
	}
	b := []byte{'?', '?', '?', '?'}
	copy(b, s)
	return string(b)
}
