// Package mux 将恢复后的轨道块确定性地再封装为 mini-WebM，
// 并提供基于独立 EBML 解读器的回读核对。
package mux

import (
	"bytes"
	"encoding/binary"
	"io"

	"bcastqc/internal/ebml"
	"bcastqc/internal/media"
)

// MuxBlock 是送入再封装器的单个轨道块。
type MuxBlock struct {
	Track    uint32
	TimeNS   int64
	Key      bool
	Payload  []byte
}

// TrackSpec 描述输出文件的轨道静态信息。
type TrackSpec struct {
	Number  uint32
	Type    uint8
}

// vint 编码一个指定宽度的 EBML 变长整数（含长度标记位）。
func vint(value uint64, width int) []byte {
	b := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		b[i] = byte(value & 0xFF)
		value >>= 8
	}
	b[0] |= 0x80 >> (width - 1)
	return b
}

func elemID(id uint32) []byte {
	// 元素 ID 宽度由数值范围决定（标记位已包含在 id 常量中）。
	width := 4
	switch {
	case id >= 1<<24:
		width = 4
	case id >= 1<<16:
		width = 3
	case id >= 1<<8:
		width = 2
	default:
		width = 1
	}
	return vint(uint64(id), width)
}

func sizeVint(size int64) []byte {
	// 各宽度的全 1 值保留给“未知长度”，因此可用上界减一。
	switch {
	case size < (1<<7)-1:
		return vint(uint64(size), 1)
	case size < (1<<14)-1:
		return vint(uint64(size), 2)
	case size < (1<<21)-1:
		return vint(uint64(size), 3)
	default:
		return vint(uint64(size), 4)
	}
}

func element(id uint32, payload []byte) []byte {
	out := append(elemID(id), sizeVint(int64(len(payload)))...)
	return append(out, payload...)
}

func uintElement(id uint32, v uint64, width int) []byte {
	b := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		b[i] = byte(v & 0xFF)
		v >>= 8
	}
	return element(id, b)
}

// Write 确定性地生成 mini-WebM 字节流。
// 块按时间分集群，输出仅依赖输入内容（不写时间戳墙钟）。
func Write(tracks []TrackSpec, blocks []MuxBlock) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeHeader(&buf); err != nil {
		return nil, err
	}
	if err := writeSegment(&buf, tracks, blocks); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeHeader(w io.Writer) error {
	body := bytes.Buffer{}
	body.Write(uintElement(ebml.IDEBMLVersion, 1, 1))
	body.Write(element(ebml.IDDocType, []byte("webm")))
	_, err := w.Write(element(ebml.IDEBML, body.Bytes()))
	return err
}

func writeSegment(w io.Writer, tracks []TrackSpec, blocks []MuxBlock) error {
	seg := bytes.Buffer{}
	// Info：固定 TimecodeScale=1ms。
	info := element(ebml.IDInfo, uintElement(ebml.IDTimecodeScale,
		uint64(media.MKTimecodeScale), 8))
	seg.Write(info)
	seg.Write(writeTracks(tracks))
	seg.Write(writeClusters(blocks))
	_, err := w.Write(element(ebml.IDSegment, seg.Bytes()))
	return err
}

func writeTracks(tracks []TrackSpec) []byte {
	body := bytes.Buffer{}
	for _, t := range tracks {
		entry := bytes.Buffer{}
		entry.Write(uintElement(ebml.IDTrackNumber, uint64(t.Number), 1))
		entry.Write(uintElement(ebml.IDTrackType, uint64(t.Type), 1))
		body.Write(element(ebml.IDTrackEntry, entry.Bytes()))
	}
	return element(ebml.IDTracks, body.Bytes())
}

// writeClusters 以 100ms 为集群边界切分块。
func writeClusters(blocks []MuxBlock) []byte {
	const clusterMS int64 = 100
	out := bytes.Buffer{}
	if len(blocks) == 0 {
		return nil
	}
	var clusterBaseNS int64 = blocks[0].TimeNS
	clusterBlocks := []MuxBlock{}
	flush := func() {
		body := bytes.Buffer{}
		baseMS := clusterBaseNS / media.MKTimecodeScale
		body.Write(uintElement(ebml.IDTimecode, uint64(baseMS), 8))
		for _, b := range clusterBlocks {
			body.Write(element(ebml.IDSimpleBlock, encodeSimpleBlock(b, baseMS)))
		}
		out.Write(element(ebml.IDCluster, body.Bytes()))
	}
	for _, b := range blocks {
		if len(clusterBlocks) > 0 && b.TimeNS-clusterBaseNS >= clusterMS*media.MKTimecodeScale {
			flush()
			clusterBlocks = nil
			clusterBaseNS = b.TimeNS
		}
		clusterBlocks = append(clusterBlocks, b)
	}
	if len(clusterBlocks) > 0 {
		flush()
	}
	return out.Bytes()
}

// encodeSimpleBlock 生成块体：VINT 轨道号 + int16 相对毫秒 + 标志 + 负载。
func encodeSimpleBlock(b MuxBlock, baseMS int64) []byte {
	relMS := b.TimeNS/media.MKTimecodeScale - baseMS
	out := []byte{}
	out = append(out, vint(uint64(b.Track), trackVIntWidth(b.Track))...)
	var tc [2]byte
	binary.BigEndian.PutUint16(tc[:], uint16(int16(relMS)))
	out = append(out, tc[:]...)
	flags := byte(0)
	if b.Key {
		flags |= 0x80
	}
	out = append(out, flags)
	out = append(out, b.Payload...)
	return out
}

func trackVIntWidth(track uint32) int {
	switch {
	case track < 1<<7-1:
		return 1
	case track < 1<<14-1:
		return 2
	default:
		return 3
	}
}
