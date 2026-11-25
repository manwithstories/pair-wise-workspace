package fixture

import (
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"mediad/internal/ingest"
)

// FaultBundle contains deterministic fault samples and injected expectations.
type FaultBundle struct {
	PCRWrap      []byte
	CCJump       []byte
	BadCRCPSI    []byte
	Truncated    []byte
	CorruptCodec []byte
	RTP          [][]byte
	RTPArrivalNS []int64
	DamageTS     []byte
	DamageOffset int64
	DamagePacket int
}

// BuildFaults constructs every deterministic fault sample from a clean pair.
func BuildFaults(seed int64, clean [2][]byte) (*FaultBundle, error) {
	damagePacket := DamagePacketIndex()
	damaged, off := DamageSync(clean[0], damagePacket)
	b := &FaultBundle{
		PCRWrap:      PCRWrapSample(),
		CCJump:       CCJumpSample(),
		BadCRCPSI:    BadCRCPSISample(),
		Truncated:    append([]byte(nil), clean[1][:len(clean[1])-37]...),
		CorruptCodec: CorruptCodecSample(),
		DamageTS:     damaged,
		DamageOffset: off,
		DamagePacket: damagePacket,
	}
	pkts, arrivals, err := RTPDuplicateReorderSample(clean[0])
	if err != nil {
		return nil, err
	}
	b.RTP = pkts
	b.RTPArrivalNS = arrivals
	return b, nil
}

// WriteFaults writes samples under dir/samples for operator inspection.
func (b *FaultBundle) WriteFaults(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "samples"), 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"pcr_wrap.ts":      b.PCRWrap,
		"cc_jump.ts":       b.CCJump,
		"bad_crc_pmt.ts":   b.BadCRCPSI,
		"truncated.ts":     b.Truncated,
		"corrupt_codec.ts": b.CorruptCodec,
		"damage_sync.ts":   b.DamageTS,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, "samples", name), data, 0o644); err != nil {
			return err
		}
	}
	for i, raw := range b.RTP {
		if err := os.WriteFile(filepath.Join(dir, "samples", "rtp_"+itoa(i)+".pkt"), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// PCRWrapSample emits PTS/PCR values crossing the 33-bit boundary.
func PCRWrapSample() []byte {
	w := NewWriter()
	w.WriteSection(PATPID, PAT(0, PMTPID))
	w.WriteSection(PMTPID, PMT(0))
	base := uint64(1<<33 - 4500)
	for i := 0; i < 6; i++ {
		pts := (base + uint64(i)*1500) & ((1 << 33) - 1)
		pcrBase := pts
		_ = w.WritePES(VideoPID, 0xe0, pts, h264PFrame(99, i, 8), &ingest.PCR{Base: pcrBase, Extension: 0})
	}
	return w.Buf
}

// CCJumpSample emits one deliberate two-count skip; the scanner adopts the
// observed counter so later contiguous packets do not create chain errors.
func CCJumpSample() []byte {
	w := NewWriter()
	w.WriteSection(PATPID, PAT(0, PMTPID))
	w.WriteSection(PMTPID, PMT(0))
	_ = w.WritePES(VideoPID, 0xe0, 0, h264KeyAccessUnit(7, 0, 8), &ingest.PCR{Base: 0})
	w.JumpCC(VideoPID, 2)
	for i := 1; i <= 5; i++ {
		_ = w.WritePES(VideoPID, 0xe0, uint64(i*3003), h264PFrame(7, i, 8), nil)
	}
	return w.Buf
}

// BadCRCPSISample contains a good PMT v0 followed by a CRC-corrupt v1 update.
func BadCRCPSISample() []byte {
	w := NewWriter()
	w.WriteSection(PATPID, PAT(0, PMTPID))
	w.WriteSection(PMTPID, PMT(0))
	bad := PMT(1)
	bad[len(bad)-2] ^= 0xff // corrupt one CRC byte while leaving version readable.
	w.WriteSection(PMTPID, bad)
	return w.Buf
}

// CorruptCodecSample is a video PES whose H.264 forbidden bit is set.
func CorruptCodecSample() []byte {
	w := NewWriter()
	w.WriteSection(PATPID, PAT(0, PMTPID))
	w.WriteSection(PMTPID, PMT(0))
	nal := []byte{0, 0, 0, 1, 0xe5, 0x01, 0x02} // 0xe5 has forbidden_zero_bit=1.
	_ = w.WritePES(VideoPID, 0xe0, 0, nal, nil)
	return w.Buf
}

// RTPDuplicateReorderSample packetizes TS packets and returns a list containing
// one duplicate and one adjacent reordering. Arrival times stay monotonic.
func RTPDuplicateReorderSample(ts []byte) ([][]byte, []int64, error) {
	const count = 10
	var ordered []*rtp.Packet
	for i := 0; i < count; i++ {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 33, SSRC: 0x73021,
				SequenceNumber: uint16(100 + i), Timestamp: uint32(i * 3003),
			},
			Payload: ts[i*ingest.PacketSize : (i+1)*ingest.PacketSize],
		}
		ordered = append(ordered, pkt)
	}
	emitOrder := []int{0, 1, 2, 2, 4, 3, 5, 6, 7, 8, 9}
	var raw [][]byte
	var arrivals []int64
	for at, idx := range emitOrder {
		b, err := ordered[idx].Marshal()
		if err != nil {
			return nil, nil, err
		}
		raw = append(raw, b)
		arrivals = append(arrivals, int64(at)*20_000_000)
	}
	return raw, arrivals, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
