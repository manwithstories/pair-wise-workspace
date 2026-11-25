package fixture

import (
	"encoding/binary"

	"github.com/pion/rtp"
)

// buildReorderedTrace 构造包含乱序与 16 位序号回绕的 RTP 记录。
// 序列顺序：0xFFFD, 0xFFFF（跳过 0xFFFE -> 丢包）, 0xFFFE（乱序）,
// 0x0000（回绕）, 0x0001，覆盖丢包、乱序、回绕三类故障。
func buildReorderedTrace(spec Spec, seq int, r *rng) []byte {
	ordered := []uint16{0xFFFD, 0xFFFF, 0xFFFE, 0x0000, 0x0001}
	ts := uint32(seq*int(spec.SegmentTicks)) + 7
	var out []byte
	for i, sn := range ordered {
		p := &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96,
				SequenceNumber: sn,
				Timestamp:      ts + uint32(i)*1000,
				SSRC:           4242,
			},
			Payload: []byte{byte(r.next()), byte(r.next())},
		}
		raw, _ := p.Marshal()
		hdr := make([]byte, 4)
		binary.BigEndian.PutUint32(hdr, uint32(len(raw)))
		out = append(out, hdr...)
		out = append(out, raw...)
	}
	return out
}
