// Package fixture 用确定性算法生成本地 WebM 样例数据，
// 不依赖任何外部媒体素材。
package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"time"

	"mediakernel/internal/ebml"
)

// 媒体结构参数。
const (
	VideoInterval = 33 * time.Millisecond
	AudioInterval = 20 * time.Millisecond
	ClusterSpan   = 500 // ms
	KeyframeEvery = 15
	VideoShare    = 0.84 // 视频码率占比
	KeyBoost      = 2.5  // 关键帧大小倍率
)

// Params 控制样例生成。
type Params struct {
	Seed        int64
	Duration    time.Duration
	BitrateKbps int
	LossBurst   int
}

// Block 是一个真值媒体块。
type Block struct {
	Track    uint64
	Index    int
	AbsNs    int64
	Keyframe bool
	Data     []byte
}

// Media 是生成结果：WebM 字节流与真值块序列。
type Media struct {
	WebM       []byte
	Blocks     []Block
	DurationNs int64
}

// Generate 生成确定性的 WebM 样例：未知长度 Segment/Cluster、
// 双轨（1=视频 2=音频）交错块。
func Generate(p Params) *Media {
	durMs := p.Duration.Milliseconds()
	nVideo := int(durMs / VideoInterval.Milliseconds())
	nAudio := int(durMs / AudioInterval.Milliseconds())

	totalBytes := int64(p.BitrateKbps) * 1000 / 8 * int64(p.Duration) / int64(time.Second)
	videoBytes := int64(float64(totalBytes) * VideoShare)
	audioBytes := int64(totalBytes) - videoBytes

	nKey := (nVideo + KeyframeEvery - 1) / KeyframeEvery
	base := videoBytes / (int64(nKey)*int64(KeyBoost*10)/10 + int64(nVideo-nKey))
	audioSize := audioBytes / int64(nAudio)

	m := &Media{}
	var videos, audios []Block
	for i := 0; i < nVideo; i++ {
		key := i%KeyframeEvery == 0
		size := base
		if key {
			size = base * int64(KeyBoost*10) / 10
		}
		videos = append(videos, Block{
			Track: 1, Index: i, AbsNs: int64(i) * int64(VideoInterval),
			Keyframe: key, Data: payload(p.Seed, 1, i, int(size)),
		})
	}
	for j := 0; j < nAudio; j++ {
		audios = append(audios, Block{
			Track: 2, Index: j, AbsNs: int64(j) * int64(AudioInterval),
			Data: payload(p.Seed, 2, j, int(audioSize)),
		})
	}
	// 归并为时间序交错块
	i, j := 0, 0
	for i < len(videos) || j < len(audios) {
		if j >= len(audios) || (i < len(videos) && videos[i].AbsNs <= audios[j].AbsNs) {
			m.Blocks = append(m.Blocks, videos[i])
			i++
		} else {
			m.Blocks = append(m.Blocks, audios[j])
			j++
		}
	}
	m.DurationNs = m.Blocks[len(m.Blocks)-1].AbsNs + int64(AudioInterval)
	m.WebM = assemble(m.Blocks)
	return m
}

// payload 生成确定性内容：sha256(seed|track|index) 链式扩展。
func payload(seed int64, track uint64, index, size int) []byte {
	var in [16]byte
	binary.BigEndian.PutUint64(in[0:8], uint64(seed))
	binary.BigEndian.PutUint32(in[8:12], uint32(track))
	binary.BigEndian.PutUint32(in[12:16], uint32(index))
	h := sha256.Sum256(in[:])
	out := make([]byte, 0, size)
	for len(out) < size {
		out = append(out, h[:]...)
		h = sha256.Sum256(h[:])
	}
	return out[:size]
}

// assemble 把块序列封装为 WebM（未知长度 Segment 与 Cluster）。
func assemble(blocks []Block) []byte {
	var b bytes.Buffer
	wElem(&b, ebml.IDEBML, []byte{0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'})
	wID(&b, ebml.IDSegment)
	b.WriteByte(0xFF) // 未知长度
	var info bytes.Buffer
	wUint(&info, ebml.IDTimecodeScale, 1_000_000)
	wElem(&b, ebml.IDInfo, info.Bytes())
	var tracks bytes.Buffer
	for _, tr := range []struct {
		num, typ uint64
		codec    string
	}{{1, 1, "V_VP8"}, {2, 2, "A_OPUS"}} {
		var e bytes.Buffer
		wUint(&e, ebml.IDTrackNumber, tr.num)
		wUint(&e, ebml.IDTrackType, tr.typ)
		wElem(&e, ebml.IDCodecID, []byte(tr.codec))
		wElem(&tracks, ebml.IDTrackEntry, e.Bytes())
	}
	wElem(&b, ebml.IDTracks, tracks.Bytes())
	clusterStart := int64(-1)
	for _, blk := range blocks {
		ms := blk.AbsNs / 1_000_000
		if ms < clusterStart || ms >= clusterStart+ClusterSpan {
			clusterStart = ms / ClusterSpan * ClusterSpan
			wID(&b, ebml.IDCluster)
			b.WriteByte(0xFF) // 未知长度
			wUint(&b, ebml.IDTimecode, uint64(clusterStart))
		}
		wBlock(&b, byte(blk.Track), int16(ms-clusterStart), blk.Keyframe, blk.Data)
	}
	return b.Bytes()
}

func wID(buf *bytes.Buffer, id uint32) {
	switch {
	case id > 0xFFFFFF:
		buf.WriteByte(byte(id >> 24))
		fallthrough
	case id > 0xFFFF:
		buf.WriteByte(byte(id >> 16))
		fallthrough
	case id > 0xFF:
		buf.WriteByte(byte(id >> 8))
		fallthrough
	default:
		buf.WriteByte(byte(id))
	}
}

func wSize(buf *bytes.Buffer, size int) {
	switch {
	case size < 0x7F:
		buf.WriteByte(byte(0x80 | size))
	case size < 0x3FFF:
		buf.WriteByte(byte(0x40 | size>>8))
		buf.WriteByte(byte(size))
	default:
		buf.WriteByte(byte(0x20 | size>>16))
		buf.WriteByte(byte(size >> 8))
		buf.WriteByte(byte(size))
	}
}

func wElem(buf *bytes.Buffer, id uint32, payload []byte) {
	wID(buf, id)
	wSize(buf, len(payload))
	buf.Write(payload)
}

func wUint(buf *bytes.Buffer, id uint32, v uint64) {
	p := []byte{byte(v)}
	for x := v >> 8; x > 0; x >>= 8 {
		p = append([]byte{byte(x)}, p...)
	}
	wElem(buf, id, p)
}

func wBlock(buf *bytes.Buffer, track byte, rel int16, key bool, payload []byte) {
	p := []byte{0x80 | track, byte(uint16(rel) >> 8), byte(uint16(rel))}
	if key {
		p = append(p, 0x80)
	} else {
		p = append(p, 0x00)
	}
	wElem(buf, ebml.IDSimpleBlock, append(p, payload...))
}
