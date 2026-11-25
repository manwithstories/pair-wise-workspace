package samplegen

import (
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"mediaarchive/internal/ingest"
)

type numberedRTP struct {
	no  int
	pkt rtp.Packet
}

func generateOpusTrack(nos []int, seed int64, salt int64) []numberedRTP {
	rng := rand.New(rand.NewSource(seed + salt*1009))
	recs := make([]numberedRTP, len(nos))
	seq := int64(65520 + salt%8)
	ts := int64(math.MaxUint32 - 4*2880)

	for i, no := range nos {
		// 在第 700 个轨内包处制造一个扩展序号和时间空洞。
		if i == 700 {
			seq++
			ts += 1920
		}

		var dur int64
		var toc byte
		switch i % 4 {
		case 0:
			dur, toc = 960, 0x08 // config 1: 20ms
		case 1:
			dur, toc = 1920, 0x10 // config 2: 40ms
		case 2:
			dur, toc = 960, 0x08
		default:
			dur, toc = 2880, 0x18 // config 3: 60ms
		}

		payload := make([]byte, 20)
		payload[0] = toc
		_, _ = rng.Read(payload[1:])
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Padding:        false,
				Extension:      false,
				PayloadType:    111,
				SequenceNumber: uint16(seq),
				Timestamp:      uint32(ts),
				SSRC:           uint32(0x0A100000 + salt),
			},
			Payload: payload,
		}
		recs[i] = numberedRTP{no: no, pkt: pkt}
		seq++
		ts += dur
	}

	// 相邻轨内包乱序；偏差小于 1 秒水位容忍窗口，因此只归并不触发回放。
	if len(recs) > 802 {
		recs[800].pkt, recs[801].pkt = recs[801].pkt, recs[800].pkt
	}

	byNo := make(map[int]int, len(nos))
	for i, n := range nos {
		byNo[n] = i
	}
	copyPrior := func(globalNo int) {
		idx, ok := byNo[globalNo]
		if !ok || idx == 0 {
			return
		}
		recs[idx].pkt = recs[idx-1].pkt
		recs[idx].no = globalNo
	}
	conflictPrior := func(globalNo int) {
		idx, ok := byNo[globalNo]
		if !ok || idx == 0 {
			return
		}
		pkt := recs[idx-1].pkt
		payload := append([]byte(nil), pkt.Payload...)
		payload[len(payload)-1] ^= 0xFF
		pkt.Payload = payload
		recs[idx].pkt = pkt
		recs[idx].no = globalNo
	}
	copyEarly := func(globalNo, earlyIndex int) {
		idx, ok := byNo[globalNo]
		if !ok || earlyIndex >= len(recs) {
			return
		}
		recs[idx].pkt = recs[earlyIndex].pkt
		recs[idx].no = globalNo
	}

	// 这些全局序号在 events=12000、三轨均分条件下都属于 track-2-opus。
	copyPrior(2003)     // 相同身份和内容：重试幂等
	conflictPrior(3002) // 相同身份、不同内容：隔离冲突
	copyEarly(5600, 10) // 很久以后重传早期包：跨水位迟到并回放
	return recs
}

func writeRTPRecords(path string, recs []numberedRTP) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, r := range recs {
		frame, err := ingest.EncodeRecord(r.no, &r.pkt)
		if err != nil {
			return err
		}
		if _, err := f.Write(frame); err != nil {
			return err
		}
	}
	return f.Sync()
}

// writeShortRTPSample 写“合法前缀 + 短 RTP + 后续合法包”，验证单包结构损坏可定位且不中断流。
func writeShortRTPSample(path string) error {
	valid := numberedRTP{no: 900001, pkt: rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: 1, Timestamp: 960, SSRC: 42},
		Payload: []byte{0x08, 0x01, 0x02, 0x03},
	}}
	frame, err := ingest.EncodeRecord(valid.no, &valid.pkt)
	if err != nil {
		return err
	}

	shortBody := make([]byte, 6)
	binary.BigEndian.PutUint32(shortBody[0:4], 900002)
	shortBody[4] = 0x80
	shortBody[5] = 0x64 // 声明 PT 但缺少完整 RTP 头
	shortFrame := make([]byte, 4+len(shortBody))
	binary.BigEndian.PutUint32(shortFrame[0:4], uint32(len(shortBody)))
	copy(shortFrame[4:], shortBody)

	after, err := ingest.EncodeRecord(900003, &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: 2, Timestamp: 1920, SSRC: 42},
		Payload: []byte{0x08, 0x04, 0x05, 0x06},
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(append(frame, shortFrame...), after...), 0o644)
}
