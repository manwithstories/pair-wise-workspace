// Package matroska 流式解析 Matroska/WebM 轨道与交错块。
// 不预扫描整个文件；负时间戳、非单调块进入统一错误模型，
// 块数据绝不被静默改写。
package matroska

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"mediakernel/internal/clock"
	"mediakernel/internal/ebml"
)

// 轨道类型。
const (
	TrackVideo = 1
	TrackAudio = 2
)

// DefaultTimecodeScale 是 Matroska 默认时间基（1ms，单位纳秒）。
const DefaultTimecodeScale = 1_000_000

// Track 描述一条媒体轨道。
type Track struct {
	Number uint64
	Type   uint64
	Codec  string
}

// Block 是一个已解析的媒体块。
type Block struct {
	Track    uint64
	Relative int16
	Abs      clock.Nanos
	Keyframe bool
	Offset   int64 // 元素在文件内的字节偏移
	Data     []byte
}

// BlockError 把解析错误与轨道、可恢复边界关联。
type BlockError struct {
	Track     uint64
	Offset    int64 // 出错字节偏移
	RecoverAt int64 // 可恢复边界（块元素起始偏移）
	Err       error
}

func (e *BlockError) Error() string {
	return fmt.Sprintf("matroska: track %d block at byte %d: %v (recover at byte %d)",
		e.Track, e.Offset, e.Err, e.RecoverAt)
}

// Unwrap 暴露底层错误。
func (e *BlockError) Unwrap() error { return e.Err }

// ErrUnsupportedLacing 表示遇到不支持的花边块。
var ErrUnsupportedLacing = errors.New("matroska: laced blocks are not supported")

// Events 接收流式解析事件；Problem 上报分类错误，解析继续。
type Events interface {
	Tracks(tracks []Track)
	Block(b Block)
	Problem(err error)
}

// Parse 流式解析 WebM 片段，事件通过 ev 回调。
func Parse(r *ebml.Reader, ev Events) error {
	p := &parser{ev: ev, scale: DefaultTimecodeScale, tl: clock.NewTimeline()}
	return p.segment(r)
}

type parser struct {
	ev     Events
	scale  uint64
	tl     *clock.Timeline
	tracks []Track
}

func (p *parser) segment(r *ebml.Reader) error {
	for {
		el, err := r.ReadElement()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch el.ID {
		case ebml.IDSegment:
			// Segment 视为延伸到流尾（兼容已知与未知长度）。
			return p.segment(r)
		case ebml.IDInfo:
			if err := p.info(r, el); err != nil {
				return err
			}
		case ebml.IDTracks:
			if err := p.readTracks(r, el); err != nil {
				return err
			}
		case ebml.IDCluster:
			if err := p.cluster(r, el); err != nil {
				return err
			}
		default:
			if err := skipKnown(r, el); err != nil {
				return err
			}
		}
	}
}

func skipKnown(r *ebml.Reader, el ebml.Element) error {
	if el.Unknown() {
		return fmt.Errorf("matroska: unknown-size element 0x%X at byte %d cannot be skipped", el.ID, el.Offset)
	}
	return r.Skip(el)
}

func (p *parser) info(r *ebml.Reader, el ebml.Element) error {
	if el.Unknown() {
		return fmt.Errorf("matroska: unknown-size Info at byte %d", el.Offset)
	}
	end := el.DataOffset + el.Size
	for r.Offset() < end {
		child, err := r.ReadElement()
		if err != nil {
			return err
		}
		if child.ID == ebml.IDTimecodeScale {
			data, err := r.ReadData(child)
			if err != nil {
				return err
			}
			p.scale = uintFromBytes(data)
		} else if err := skipKnown(r, child); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) readTracks(r *ebml.Reader, el ebml.Element) error {
	if el.Unknown() {
		return fmt.Errorf("matroska: unknown-size Tracks at byte %d", el.Offset)
	}
	end := el.DataOffset + el.Size
	for r.Offset() < end {
		child, err := r.ReadElement()
		if err != nil {
			return err
		}
		if child.ID != ebml.IDTrackEntry {
			if err := skipKnown(r, child); err != nil {
				return err
			}
			continue
		}
		tr, err := p.trackEntry(r, child)
		if err != nil {
			return err
		}
		p.tracks = append(p.tracks, tr)
	}
	p.ev.Tracks(p.tracks)
	return nil
}

func (p *parser) trackEntry(r *ebml.Reader, el ebml.Element) (Track, error) {
	var tr Track
	end := el.DataOffset + el.Size
	for r.Offset() < end {
		child, err := r.ReadElement()
		if err != nil {
			return tr, err
		}
		switch child.ID {
		case ebml.IDTrackNumber, ebml.IDTrackType:
			data, err := r.ReadData(child)
			if err != nil {
				return tr, err
			}
			if child.ID == ebml.IDTrackNumber {
				tr.Number = uintFromBytes(data)
			} else {
				tr.Type = uintFromBytes(data)
			}
		case ebml.IDCodecID:
			data, err := r.ReadData(child)
			if err != nil {
				return tr, err
			}
			tr.Codec = string(data)
		default:
			if err := skipKnown(r, child); err != nil {
				return tr, err
			}
		}
	}
	return tr, nil
}

func (p *parser) cluster(r *ebml.Reader, el ebml.Element) error {
	var base int64
	end := el.DataOffset + el.Size
	for {
		if !el.Unknown() && r.Offset() >= end {
			return nil
		}
		child, err := r.ReadElement()
		if err == io.EOF {
			return nil // 未知长度 Cluster 延伸到流尾
		}
		if err != nil {
			return err
		}
		switch child.ID {
		case ebml.IDCluster, ebml.IDSegment, ebml.IDEBML, ebml.IDTracks, ebml.IDInfo:
			// 未知长度 Cluster 的结束标志：同级元素出现。
			r.PushBack(child)
			return nil
		case ebml.IDTimecode:
			data, err := r.ReadData(child)
			if err != nil {
				return err
			}
			base = int64(uintFromBytes(data))
		case ebml.IDSimpleBlock:
			data, err := r.ReadData(child)
			if err != nil {
				return &BlockError{Offset: child.DataOffset, RecoverAt: child.Offset, Err: err}
			}
			p.simpleBlock(child, base, data)
		case ebml.IDBlockGroup:
			if err := p.blockGroup(r, child, base); err != nil {
				return err
			}
		default:
			if err := skipKnown(r, child); err != nil {
				return err
			}
		}
	}
}

func (p *parser) blockGroup(r *ebml.Reader, el ebml.Element, base int64) error {
	if el.Unknown() {
		return fmt.Errorf("matroska: unknown-size BlockGroup at byte %d", el.Offset)
	}
	end := el.DataOffset + el.Size
	for r.Offset() < end {
		child, err := r.ReadElement()
		if err != nil {
			return err
		}
		if child.ID == ebml.IDBlock {
			data, err := r.ReadData(child)
			if err != nil {
				return &BlockError{Offset: child.DataOffset, RecoverAt: child.Offset, Err: err}
			}
			p.simpleBlock(child, base, data)
		} else if err := skipKnown(r, child); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) simpleBlock(el ebml.Element, base int64, data []byte) {
	track, n := vintValue(data)
	rest := len(data) - n
	if n == 0 || rest < 3 {
		p.ev.Problem(&BlockError{Offset: el.DataOffset, RecoverAt: el.Offset, Err: io.ErrUnexpectedEOF})
		return
	}
	rel := int16(binary.BigEndian.Uint16(data[n:]))
	flags := data[n+2]
	if flags&0x06 != 0 {
		p.ev.Problem(&BlockError{Track: track, Offset: el.DataOffset, RecoverAt: el.Offset, Err: ErrUnsupportedLacing})
		return
	}
	abs := clock.TicksToNanos(base+int64(rel), int64(p.scale), 1)
	if err := p.tl.Validate(track, abs); err != nil {
		p.ev.Problem(err) // 上报统一错误模型，块按原样下发
	}
	p.ev.Block(Block{
		Track:    track,
		Relative: rel,
		Abs:      abs,
		Keyframe: flags&0x80 != 0,
		Offset:   el.Offset,
		Data:     data[n+3:],
	})
}

// vintValue 解析 EBML 变长整数（去除标记位）。
func vintValue(data []byte) (uint64, int) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0
	}
	width := 1
	mask := byte(0x80)
	for data[0]&mask == 0 {
		width++
		mask >>= 1
	}
	if len(data) < width {
		return 0, 0
	}
	val := uint64(data[0] &^ mask)
	for i := 1; i < width; i++ {
		val = val<<8 | uint64(data[i])
	}
	return val, width
}

func uintFromBytes(data []byte) uint64 {
	var v uint64
	for _, b := range data {
		v = v<<8 | uint64(b)
	}
	return v
}
