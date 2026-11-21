// Package fixture 自生成演示与测试样例：正常/截断清单、损坏 box、
// 乱序/重复/丢失 RTP、静音及非整帧 PCM。全部确定性生成，不引用外部文件。
package fixture

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"

	"github.com/pion/rtp"

	"mediadeliver/internal/audio"
	"mediadeliver/internal/pipeline"
)

// Set 是一组夹具。
type Set struct {
	ManifestGood  []byte // 正常 HLS 清单
	ManifestTrunc []byte // 截断清单（尾部 EXTINF 缺 URI）
	Segments      []pipeline.SegmentData
	CorruptIndex  int      // 损坏 box 所在分片
	Corrupt       []byte   // 该分片的损坏变体
	ShortTailIdx  int      // 非整帧 PCM 所在分片（恢复路径注入）
	ShortTailAudio audio.PCM
	RTPGood       [][]byte // 窗口内乱序 + 重复
	RTPLossy      [][]byte // 乱序 + 重复 + 丢失
}

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

// InitSegment 生成最小初始化段（ftyp+moov）。
func InitSegment() []byte {
	out := box("ftyp", []byte("isom\x00\x00\x02\x00isomiso6"))
	out = append(out, box("moov", box("mvhd", make([]byte, 100)))...)
	return out
}

// MediaSegment 生成媒体段（sidx+mdat）；corrupt 时在中间放入非法 box。
func MediaSegment(seed int64, refs int, corrupt bool) []byte {
	rnd := rand.New(rand.NewSource(seed))
	sizes := make([]uint32, refs)
	payload := 0
	for i := range sizes {
		sizes[i] = uint32(200 + rnd.Intn(200))
		payload += int(sizes[i])
	}
	sidx := make([]byte, 0, 20+12*refs)
	sidx = append(sidx, 0, 0, 0, 0, 0, 0, 0, 1) // v0+flags, refID
	sidx = append(sidx, 0, 0, 0xbb, 0x80)       // timescale 48000
	sidx = append(sidx, make([]byte, 10)...)    // earliest+offset+reserved
	sidx = append(sidx, byte(refs>>8), byte(refs))
	for _, s := range sizes {
		r := make([]byte, 12)
		binary.BigEndian.PutUint32(r, s)
		binary.BigEndian.PutUint32(r[4:], 96000) // 2s @48k
		sidx = append(sidx, r...)
	}
	mdat := box("mdat", make([]byte, payload))
	if corrupt {
		out := box("sidx", sidx)
		out = append(out, 0, 0, 0, 3, 'b', 'a', 'd', '!') // 非法长度
		return append(out, mdat...)
	}
	return append(box("sidx", sidx), mdat...)
}

// sinePCM 生成单声道正弦源（44.1kHz，演示重采样与单声道复制）。
func sinePCM(rate int64, frames int, amp float64) audio.PCM {
	p := audio.PCM{SampleRate: rate, Channels: 1, Frames: make([]float64, frames)}
	for i := range p.Frames {
		p.Frames[i] = amp * math.Sin(2*math.Pi*440*float64(i)/float64(rate))
	}
	return p
}

// rtpStream 生成 RTP 流；mode: 0 正常(乱序+重复) 1 有损(再叠加丢失)。
func rtpStream(seed int64, n int, mode int) [][]byte {
	rnd := rand.New(rand.NewSource(seed))
	seqs := make([]uint16, n)
	for i := range seqs {
		seqs[i] = uint16(1000 + i)
	}
	// 窗口内相邻交换制造乱序
	for i := 0; i+1 < n; i += 7 {
		seqs[i], seqs[i+1] = seqs[i+1], seqs[i]
	}
	var out [][]byte
	mk := func(s uint16) []byte {
		p := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: s,
			Timestamp: uint32((int(s) - 1000) * 960), SSRC: 7}}
		raw, _ := p.Marshal()
		return raw
	}
	dropped := map[int]bool{}
	if mode == 1 {
		dropped[rnd.Intn(n/2)] = true // 丢失 1 个
	}
	for i, s := range seqs {
		if dropped[i] {
			continue
		}
		out = append(out, mk(s))
		if i%11 == 0 {
			out = append(out, mk(s)) // 重复
		}
	}
	return out
}

// Generate 生成完整夹具集。
func Generate(seed int64, numSegs int, segDurSec float64) Set {
	s := Set{CorruptIndex: 1, ShortTailIdx: numSegs - 1}
	frames := int(segDurSec * 44100)
	manifest := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n"
	for i := 0; i < numSegs; i++ {
		uri := fmt.Sprintf("seg%d.m4s", i)
		manifest += fmt.Sprintf("#EXTINF:%.6f,\n%s\n", segDurSec, uri)
		s.Segments = append(s.Segments, pipeline.SegmentData{
			URI: uri, Fragment: MediaSegment(seed+int64(i), 2, false),
			Audio: sinePCM(44100, frames, 0.2),
		})
	}
	// 非整帧 PCM：立体声源挂 1 个零散采样（恢复路径注入用）
	st := audio.PCM{SampleRate: 48000, Channels: 2,
		Frames: make([]float64, 2*int(segDurSec*48000)+1)}
	for j := 0; j+1 < len(st.Frames); j += 2 {
		st.Frames[j], st.Frames[j+1] = 0.1, 0.1
	}
	s.ShortTailAudio = st
	s.ManifestGood = []byte(manifest + "#EXT-X-ENDLIST\n")
	// 截断清单：最后一个 EXTINF 无 URI、无 ENDLIST
	trunc := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n"
	for i := 0; i < numSegs-1; i++ {
		trunc += fmt.Sprintf("#EXTINF:%.6f,\nseg%d.m4s\n", segDurSec, i)
	}
	trunc += fmt.Sprintf("#EXTINF:%.6f,\n", segDurSec)
	s.ManifestTrunc = []byte(trunc)
	s.Corrupt = MediaSegment(seed+int64(s.CorruptIndex), 2, true)
	s.RTPGood = rtpStream(seed, 40, 0)
	s.RTPLossy = rtpStream(seed, 40, 1)
	return s
}
