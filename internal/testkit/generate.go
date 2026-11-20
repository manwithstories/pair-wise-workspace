// Package testkit 生成正常、损坏及边界样例，全部内容仅由种子决定，
// 保证固定种子重复执行得到相同校验和。
package testkit

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	mgrtp "mediagraph/internal/rtp"

	"github.com/pion/rtp"
)

// 夹具文件名（同时作为检查点输入摘要的成员）。
const (
	GoodSRT    = "good.srt"
	CorruptSRT = "corrupt.srt"
	GoodVTT    = "good.vtt"
	RTPGood    = "rtp_good.bin"
	RTPCorrupt = "rtp_corrupt.bin"
	MP4Good    = "good.mp4"
	MP4Corrupt = "corrupt.mp4"
	Timecodes  = "timecodes.txt"
	Levels     = "levels.txt"
)

// Fixtures 描述一次生成的样例集合。
type Fixtures struct {
	Dir         string
	Files       []string // 参与输入摘要的相对文件名
	BucketTicks int64    // 音量每桶刻度数
	MediaTicks  int64    // 媒体总时长（主基准刻度）
	LevelCount  int
}

// All 返回全部夹具文件名（有序，确定性）。
func All() []string {
	return []string{GoodSRT, CorruptSRT, GoodVTT, RTPGood, RTPCorrupt, MP4Good, MP4Corrupt, Timecodes, Levels}
}

// Generate 在 dir 下生成全部样例；同一种子输出逐字节一致。
func Generate(dir string, seed int64) (*Fixtures, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(seed))
	write := func(name string, data []byte) error {
		return os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}

	// --- 正常 SRT：12 条不重叠字幕 ---
	var good bytes.Buffer
	for i := 0; i < 12; i++ {
		start := int64(i) * 2500
		fmt.Fprintf(&good, "%d\n00:00:%02d,%03d --> 00:00:%02d,%03d\ncue %d seed %d\n\n",
			i+1, start/1000, start%1000, (start+2000)/1000, (start+2000)%1000, i+1, rng.Int63n(1<<30))
	}
	if err := write(GoodSRT, good.Bytes()); err != nil {
		return nil, err
	}

	// --- 损坏 SRT：非法时间 → 截断块 → 重叠 → 可恢复字幕 ---
	corrupt := "1\n00:00:01,000 --> 00:00:02,000\nlead ok\n\n" +
		"2\n99:99:99,999 --> 00:00:03,000\nbad time\n\n" +
		"3\n00:00:03,000 --> 00:00:04,000\n\n" +
		"4\n00:00:03,000 --> 00:00:04,000\nanchor ok\n\n" +
		"5\n00:00:03,500 --> 00:00:04,500\noverlap cue\n\n" +
		"6\n00:00:05,000 --> 00:00:06,000\nrecovered one\n\n" +
		"7\n00:00:06,500 --> 00:00:07,500\nrecovered two\n"
	if err := write(CorruptSRT, []byte(corrupt)); err != nil {
		return nil, err
	}

	// --- 正常 WebVTT ---
	vtt := "WEBVTT\n\n00:01.000 --> 00:02.000\nvtt one\n\n00:03.000 --> 00:04.500\nvtt two\n"
	if err := write(GoodVTT, []byte(vtt)); err != nil {
		return nil, err
	}

	// --- RTP：正常流与损坏流 ---
	if err := write(RTPGood, rtpStream([]uint16{1000, 1001, 1002, 1003, 1004, 1005, 1006, 1007},
		[]uint32{90000, 93000, 96000, 99000, 102000, 105000, 108000, 111000})); err != nil {
		return nil, err
	}
	if err := write(RTPCorrupt, rtpStream(
		[]uint16{2000, 2001, 2003, 2003, 2002, 2004, 2005, 2006},
		[]uint32{1000, 2000, 3000, 3000, 2500, 0xFFFFFF00, 0x00000050, 100})); err != nil {
		return nil, err
	}

	// --- MP4：正常与损坏（计数不符 + chunk 越界）---
	sizes := []uint32{10, 10, 10}
	goodMP4, err := BuildGoodMP4(90000, 3, sizes, 30)
	if err != nil {
		return nil, err
	}
	if err := write(MP4Good, goodMP4); err != nil {
		return nil, err
	}
	badMP4, err := BuildMP4(90000, 3, 4, []uint32{10, 10, 10, 10}, 1<<20, 30)
	if err != nil {
		return nil, err
	}
	if err := write(MP4Corrupt, badMP4); err != nil {
		return nil, err
	}

	// --- 时间码：多制式、负偏移、日界回绕、一条非法 ---
	tc := "25 01:00:00:00\n30000/1001 00:00:01:00\n30 -00:00:02:00\n25 25:00:00:01\n24 23:59:59:23\n25 99:99:99:99\n"
	if err := write(Timecodes, []byte(tc)); err != nil {
		return nil, err
	}

	// --- 音量序列：60 桶 * 500ms = 30s，含两段静音与阈值附近抖动 ---
	var lv bytes.Buffer
	pattern := []float64{}
	for i := 0; i < 60; i++ {
		switch {
		case i >= 10 && i < 16: // 静音段 1（5-8s）
			pattern = append(pattern, 0.02+0.01*rng.Float64())
		case i >= 20 && i < 24: // 滞回带抖动（10-12s）
			pattern = append(pattern, 0.15+0.1*rng.Float64())
		case i >= 40 && i < 48: // 静音段 2（20-24s）
			pattern = append(pattern, 0.01+0.02*rng.Float64())
		default:
			pattern = append(pattern, 0.7+0.2*rng.Float64())
		}
	}
	for _, v := range pattern {
		fmt.Fprintf(&lv, "%.4f\n", v)
	}
	if err := write(Levels, lv.Bytes()); err != nil {
		return nil, err
	}

	return &Fixtures{
		Dir:         dir,
		Files:       All(),
		BucketTicks: 500 * 27000, // 500ms
		MediaTicks:  60 * 500 * 27000,
		LevelCount:  60,
	}, nil
}

// rtpStream 生成确定性的本地 RTP 记录流。
func rtpStream(seqs []uint16, tss []uint32) []byte {
	var buf bytes.Buffer
	for i := range seqs {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seqs[i], Timestamp: tss[i], SSRC: 4242},
			Payload: []byte{byte(i), 0xAB, 0xCD},
		}
		// 生成环境忽略错误：包由本函数构造，必然合法
		_ = mgrtp.Encode(&buf, pkt)
	}
	return buf.Bytes()
}
