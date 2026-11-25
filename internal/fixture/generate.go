package fixture

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// 场景内的特殊分片序号（12 分片夹具）。
const (
	missingSeg   = 5  // 缺片：清单引用但物理文件缺失
	truncatedSeg = 7  // 截断盒：mdat 被砍半
	reorderSeg   = 9  // RTP 乱序/回绕
	oversizedSeg = 11 // 超长分片（超过 --mem）
)

// Generate 在 outDir 下生成一整套夹具。edge=true 时注入各类传输故障。
func Generate(outDir string, spec Spec, edge bool) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	r := newRNG(spec.Seed)

	// 分片与 RTP 记录。
	for seq := 0; seq < spec.SegmentCount; seq++ {
		if edge && seq == missingSeg {
			continue // 物理文件缺失，但清单仍引用
		}
		size := 256
		if edge && seq == oversizedSeg {
			size = 1200 // 明显大于常规分片，配合 analyze 的受限内存预算触发超长故障
		}
		frag := buildFragment(spec, seq, r, size)
		if edge && seq == truncatedSeg {
			frag = frag[:len(frag)*3/4] // 砍掉 mdat 尾部，盒头长度不变
		}
		if err := os.WriteFile(filepath.Join(outDir, segName(seq)), frag, 0o644); err != nil {
			return err
		}

		inject := edge // 时间戳跳变
		var rtp []byte
		if edge && seq == reorderSeg {
			rtp = buildReorderedTrace(spec, seq, r)
		} else {
			rtp = buildRTPTrace(spec, seq, r, uint16(seq*spec.RTPPerSegment), inject)
		}
		if err := os.WriteFile(filepath.Join(outDir, rtpName(seq)), rtp, 0o644); err != nil {
			return err
		}
	}

	if err := writeHLS(outDir, spec); err != nil {
		return err
	}
	if err := writeDASH(outDir, spec); err != nil {
		return err
	}
	if edge {
		return writeCorruptCheckpoint(outDir)
	}
	return nil
}

// writeHLS 生成包含全部分片引用的媒体播放清单。
func writeHLS(dir string, spec Spec) error {
	durSec := float64(spec.SegmentTicks) / float64(spec.TimeScale)
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MEDIA-SEQUENCE:0\n"...)
	b = append(b, "#EXT-X-TARGETDURATION:1\n"...)
	for seq := 0; seq < spec.SegmentCount; seq++ {
		b = append(b, fmt.Sprintf("#EXTINF:%s,\n", strconv.FormatFloat(durSec, 'f', 3, 64))...)
		b = append(b, segName(seq)+"\n"...)
	}
	b = append(b, "#EXT-X-ENDLIST\n"...)
	return os.WriteFile(filepath.Join(dir, "playlist.m3u8"), b, 0o644)
}

// writeDASH 生成 SegmentTemplate($Number$)+SegmentTimeline 的 MPD。
func writeDASH(dir string, spec Spec) error {
	var b []byte
	b = append(b, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static">`+"\n"...)
	b = append(b, `  <Period><AdaptationSet>`+"\n"...)
	b = append(b, fmt.Sprintf(
		`    <SegmentTemplate timescale="%d" media="seg$Number$.m4s" startNumber="0">`+"\n",
		spec.TimeScale)...)
	b = append(b, `      <SegmentTimeline>`+"\n"...)
	b = append(b, fmt.Sprintf(`        <S t="0" d="%d" r="%d"/>`+"\n",
		spec.SegmentTicks, spec.SegmentCount-1)...)
	b = append(b, "      </SegmentTimeline>\n    </SegmentTemplate>\n"...)
	b = append(b, "  </AdaptationSet></Period>\n</MPD>\n"...)
	return os.WriteFile(filepath.Join(dir, "manifest.mpd"), b, 0o644)
}

// writeCorruptCheckpoint 写一个内容与内嵌校验和不符的损坏检查点样本。
func writeCorruptCheckpoint(dir string) error {
	payload := []byte(`{"body":"bm90LXNvLWZhc3Q=","sha256":"deadbeef"}`)
	return os.WriteFile(filepath.Join(dir, "corrupt-checkpoint.json"), payload, 0o644)
}
