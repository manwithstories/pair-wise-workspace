package ebml

import (
	"encoding/binary"
	"io"

	"bcastqc/internal/media"
)

// 轨道类型（Matroska TrackType）。
const (
	TrackTypeVideo = 1
	TrackTypeAudio = 2
)

// TrackInfo 描述一条轨道的静态属性。
type TrackInfo struct {
	Number   uint32
	Type     uint8
	ClockNS  int64 // 单采样纳秒数（由默认时长推导，未知为 0）
}

// Block 是从 SimpleBlock/Block 中提取的单个轨道块。
type Block struct {
	Track    uint32
	TimeNS   int64 // 集群时间码 + 块相对时间码，统一为有符号纳秒
	Keyframe bool
	Payload  []byte
}

// Document 是 mini-WebM 解读结果。
type Document struct {
	TimecodeScale int64 // 纳秒/时间码单位
	Tracks        []TrackInfo
	Blocks        []Block
}

// ParseDocument 顺序解读整份 mini-WebM：按父级边界下钻，
// 任何越界或截断立即停止并返回带首个缺失偏移的错误。
func ParseDocument(r *Reader) (*Document, error) {
	doc := &Document{TimecodeScale: media.MKTimecodeScale}
	for {
		el, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch el.ID {
		case IDEBML:
			if err := r.Drain(el); err != nil {
				return nil, err
			}
		case IDSegment:
			if err := parseSegment(r, el, doc); err != nil {
				return nil, err
			}
		default:
			if err := r.Drain(el); err != nil {
				return nil, err
			}
		}
	}
	return doc, nil
}

func parseSegment(r *Reader, seg *Element, doc *Document) error {
	sr := r.Child(seg)
	for {
		el, err := sr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch el.ID {
		case IDSeekHead, IDInfo:
			if err := parseInfo(sr, el, doc); err != nil {
				return err
			}
		case IDTracks:
			if err := parseTracks(sr, el, doc); err != nil {
				return err
			}
		case IDCluster:
			if err := parseCluster(sr, el, doc); err != nil {
				return err
			}
		default:
			if err := sr.Drain(el); err != nil {
				return err
			}
		}
	}
	return r.Drain(seg)
}

// parseInfo 同时兼容直接的 Info 元素与 SeekHead 包装。
func parseInfo(r *Reader, el *Element, doc *Document) error {
	if el.ID != IDInfo {
		return r.Drain(el)
	}
	ir := r.Child(el)
	for {
		child, err := ir.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if child.ID == IDTimecodeScale {
			data, err := ir.ReadData(child)
			if err != nil {
				return err
			}
			doc.TimecodeScale = int64(binary.BigEndian.Uint64(pad8(data)))
		} else if err := ir.Drain(child); err != nil {
			return err
		}
	}
	return r.Drain(el)
}

func parseTracks(r *Reader, tracks *Element, doc *Document) error {
	tr := r.Child(tracks)
	for {
		el, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if el.ID != IDTrackEntry {
			if err := tr.Drain(el); err != nil {
				return err
			}
			continue
		}
		ti := TrackInfo{}
		er := tr.Child(el)
		for {
			child, err := er.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			data, err := er.ReadData(child)
			if err != nil {
				return err
			}
			switch child.ID {
			case IDTrackNumber:
				ti.Number = uint34(data)
			case IDTrackType:
				if len(data) > 0 {
					ti.Type = data[0]
				}
			case IDDefaultDuration:
				ti.ClockNS = int64(binary.BigEndian.Uint64(pad8(data)))
			}
		}
		doc.Tracks = append(doc.Tracks, ti)
	}
	return r.Drain(tracks)
}

func parseCluster(r *Reader, cluster *Element, doc *Document) error {
	cr := r.Child(cluster)
	var clusterTC int64
	for {
		el, err := cr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch el.ID {
		case IDTimecode:
			data, err := cr.ReadData(el)
			if err != nil {
				return err
			}
			clusterTC = int64(uint34(data))
		case IDSimpleBlock, IDBlock:
			data, err := cr.ReadData(el)
			if err != nil {
				return err
			}
			blk, err := decodeBlock(data, clusterTC, doc.TimecodeScale, el.ID == IDSimpleBlock)
			if err != nil {
				return err
			}
			doc.Blocks = append(doc.Blocks, blk)
		default:
			if err := cr.Drain(el); err != nil {
				return err
			}
		}
	}
	return r.Drain(cluster)
}

// decodeBlock 解析 Matroska 块头：VINT 轨道号、int16 相对时间码、
// 标志字节（仅 SimpleBlock 含关键帧位）。
func decodeBlock(data []byte, clusterTC, scale int64, simple bool) (Block, error) {
	if len(data) < 4 {
		return Block{}, media.NewError(media.StageParse, media.KindTruncated,
			"块头不足 4 字节").WithOffset(-1)
	}
	track, n := decodeVInt(data)
	if n <= 0 || n+3 > len(data) {
		return Block{}, media.NewError(media.StageParse, media.KindTruncated,
			"块轨道号 VINT 截断")
	}
	rel := int64(int16(binary.BigEndian.Uint16(data[n : n+2])))
	flags := data[n+2]
	payload := data[n+3:]
	keyframe := false
	if simple {
		keyframe = flags&0x80 != 0
	}
	return Block{
		Track:    track,
		TimeNS:   (clusterTC + rel) * scale,
		Keyframe: keyframe,
		Payload:  payload,
	}, nil
}

// decodeVInt 解码数据内 VINT（去掉长度标记位），返回值与占用字节数。
func decodeVInt(b []byte) (uint32, int) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0
	}
	width := 1
	for width <= 4 && b[0]&(0x80>>(width-1)) == 0 {
		width++
	}
	if width > len(b) {
		return 0, 0
	}
	v := uint32(b[0]) & (0xFF >> width)
	for i := 1; i < width; i++ {
		v = (v << 8) | uint32(b[i])
	}
	return v, width
}

func uint34(b []byte) uint32 {
	if len(b) > 4 {
		b = b[len(b)-4:]
	}
	var v uint32
	for _, x := range b {
		v = (v << 8) | uint32(x)
	}
	return v
}

func pad8(b []byte) []byte {
	if len(b) >= 8 {
		return b[len(b)-8:]
	}
	out := make([]byte, 8)
	copy(out[8-len(b):], b)
	return out
}
