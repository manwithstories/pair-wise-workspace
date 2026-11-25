package fixture

import (
	"encoding/binary"

	"github.com/pion/rtp"
)

const (
	trunDataOffset     uint32 = 0x000001
	trunSampleDuration uint32 = 0x000100
	trunSampleSize     uint32 = 0x000200
	trunSampleFlags    uint32 = 0x000400
	sampleFlagSync     uint32 = 0x02000000 // sample_depends_on=2
	sampleFlagDep      uint32 = 0x01000000
)

// buildFragment 构造一个合法的 fMP4 分片（moof/traf/tfdt/tfhd/trun + mdat）。
// 每个分片首个样本为关键帧，baseDecodeTime 由序号与时间尺度决定。
func buildFragment(spec Spec, seq int, r *rng, sampleSize int) []byte {
	mfhd := fullBox("mfhd", 0, 0, u32(uint32(seq)))
	tfhd := fullBox("tfhd", 0, 0x020000, u32(1))
	base := uint64(seq) * uint64(spec.SegmentTicks)
	tfdt := fullBox("tfdt", 1, 0, u64(base))

	// trun 条目：duration + size + flags。
	tickPerSample := spec.SegmentTicks / uint32(spec.SamplesPerSeg)
	var entries []byte
	var payload []byte
	totalSize := 0
	for i := 0; i < spec.SamplesPerSeg; i++ {
		sz := sampleSize
		flags := sampleFlagDep
		if i == 0 {
			flags = sampleFlagSync // 每分片一个关键帧
		}
		entries = append(entries, u32(tickPerSample)...)
		entries = append(entries, u32(uint32(sz))...)
		entries = append(entries, u32(flags)...)
		sample := make([]byte, sz)
		for j := range sample {
			sample[j] = byte(r.next())
		}
		payload = append(payload, sample...)
		totalSize += sz
	}
	trunBody := append(u32(uint32(spec.SamplesPerSeg)), u32(0)...) // count + dataOffset 占位
	trunBody = append(trunBody, entries...)
	trun := fullBox("trun", 0,
		trunDataOffset|trunSampleDuration|trunSampleSize|trunSampleFlags, trunBody)
	traf := box("traf", append(append(tfhd, tfdt...), trun...))
	moof := box("moof", append(mfhd, traf...))
	mdat := box("mdat", payload)
	frag := append(moof, mdat...)

	// 回填相对 moof 的 DataOffset。
	dataOffset := len(moof) + 8
	trunOff := 8 /*moof*/ + len(mfhd) + 8 /*traf*/ + len(tfhd) + len(tfdt) +
		8 /*trun hdr*/ + 4 /*fullbox*/ + 4 /*count*/
	binary.BigEndian.PutUint32(frag[trunOff:trunOff+4], uint32(dataOffset))
	_ = totalSize
	return frag
}

// buildRTPTrace 构造带 4 字节长度前缀的 RTP 诊断记录。
// startSeq 为起始 16 位序号；ticksPerPacket 控制时间戳步进。
func buildRTPTrace(spec Spec, seq int, r *rng, startSeq uint16, injectFault bool) []byte {
	var out []byte
	ts := uint32(seq*int(spec.SegmentTicks)) + 1
	for i := 0; i < spec.RTPPerSegment; i++ {
		sn := uint16(int(startSeq) + i)
		p := &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96,
				SequenceNumber: sn, Timestamp: ts, SSRC: 4242,
			},
			Payload: []byte{byte(r.next()), byte(r.next())},
		}
		raw, _ := p.Marshal()
		hdr := u32(uint32(len(raw)))
		out = append(out, hdr...)
		out = append(out, raw...)
		ts += spec.SegmentTicks / uint32(spec.RTPPerSegment)
	}
	if injectFault {
		injectRTPFault(&out, r, spec, startSeq, ts)
	}
	return out
}

// injectRTPFault 注入一个时间戳跳变包，模拟编码器重启后的时间基不连续。
func injectRTPFault(out *[]byte, r *rng, spec Spec, startSeq uint16, ts uint32) {
	sn := uint16(int(startSeq) + spec.RTPPerSegment)
	p := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96,
			SequenceNumber: sn, Timestamp: ts + 6_000_000, SSRC: 4242,
		},
		Payload: []byte{byte(r.next()), byte(r.next())},
	}
	raw, _ := p.Marshal()
	*out = append(*out, u32(uint32(len(raw)))...)
	*out = append(*out, raw...)
}
