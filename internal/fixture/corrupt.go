package fixture

import (
	"fmt"
	"os"
	"path/filepath"
)

// CorruptSet 损坏样例集合。
type CorruptSet struct {
	NonMonotonicMPD string // 非单调时间戳 MPD
	MissingInitM3U8 string // 缺失初始化段的 HLS
	TruncatedSegDir string // 含截断分段的节目目录
	RollbackPackets []Packet
}

// GenerateCorrupt 生成损坏样例。
func GenerateCorrupt(dir string, o Options) (*CorruptSet, error) {
	cs := &CorruptSet{}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// 1. 非单调时间戳 MPD
	cs.NonMonotonicMPD = filepath.Join(dir, "nonmono.mpd")
	mpd := `<?xml version="1.0"?>
<MPD><Period><AdaptationSet>
  <Representation id="a1" bandwidth="128000">
    <SegmentTemplate media="seg_$Number$.m4s" initialization="init.mp4" timescale="48000">
      <SegmentTimeline>
        <S t="96000" d="96000"/>
        <S t="48000" d="96000"/>
      </SegmentTimeline>
    </SegmentTemplate>
  </Representation>
</AdaptationSet></Period></MPD>`
	if err := os.WriteFile(cs.NonMonotonicMPD, []byte(mpd), 0o644); err != nil {
		return nil, err
	}
	// 2. 缺失初始化段的 HLS
	cs.MissingInitM3U8 = filepath.Join(dir, "noinit.m3u8")
	hls := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nseg0.ts\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(cs.MissingInitM3U8, []byte(hls), 0o644); err != nil {
		return nil, err
	}
	// 3. 截断分段节目（清单声明完整、文件被截断）
	cs.TruncatedSegDir = filepath.Join(dir, "trunc")
	if err := os.MkdirAll(cs.TruncatedSegDir, 0o755); err != nil {
		return nil, err
	}
	prog, err := Generate(cs.TruncatedSegDir, o)
	if err != nil {
		return nil, err
	}
	last := filepath.Join(cs.TruncatedSegDir, prog.SegmentFiles[len(prog.SegmentFiles)-1])
	b, err := os.ReadFile(last)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(last, b[:len(b)/2], 0o644); err != nil { // 截断一半
		return nil, err
	}
	// 4. 时间戳回退包流
	full := sinePCM(o.Rate, o.Channels, o.SegFrames, 0, o.Amp)
	fs := o.FrameSamples()
	frameBytes := fs * o.Channels * 2
	var seq uint16
	for off := 0; off+frameBytes <= len(full) && seq < 10; off += frameBytes {
		ts := uint32(seq) * uint32(fs)
		if seq == 5 {
			ts = 0 // 回退
		}
		cs.RollbackPackets = append(cs.RollbackPackets, Packet{
			Seq: seq, TS: ts, Payload: full[off : off+frameBytes],
		})
		seq++
	}
	return cs, nil
}

// String 便于日志定位（不输出媒体载荷）。
func (cs *CorruptSet) String() string {
	return fmt.Sprintf("corrupt{mpd=%s m3u8=%s trunc=%s rollback_pkts=%d}",
		cs.NonMonotonicMPD, cs.MissingInitM3U8, cs.TruncatedSegDir, len(cs.RollbackPackets))
}
