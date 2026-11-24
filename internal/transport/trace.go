// Package transport reads captured RTP packet records and reconstructs the
// byte stream of each delivered media fragment inside a bounded reorder
// window. Packets arriving outside the window are never filled in silently;
// every unrecovered gap is located to a representation and time range.
package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/pion/rtp"

	"mediac/internal/core"
)

// Read bounds mirror the manifest limits: a hostile capture must not be able
// to exhaust memory.
const (
	MaxRecordBytes = 64 << 20 // 64 MiB per .rtpr record file
	MaxPackets     = 200_000
	// DefaultReorderWindow is the maximum span of RTP sequence numbers held
	// while waiting for late packets before a gap is declared permanent.
	DefaultReorderWindow = 64
	rtprMagic            = "RTPR"
	rtprVersion          = 1
	headerSize           = 12
)

// RecordPacket is one captured RTP packet plus the capture metadata needed to
// distinguish delivery loss from material defects. Fragment and payload
// Offset are carried in a one-element RTP header extension (id 1):
// extension data = fragment(16 bits) | payload offset(32 bits), 6 bytes.
type RecordPacket struct {
	ArrivalUS int64 // capture timestamp in microseconds
	RepID     string
	Fragment  int    // fragment/segment sequence within the representation
	Offset    uint32 // byte offset of the payload inside the fragment
	Packet    *rtp.Packet
}

// ExtensionID is the RTP header-extension id reserved for fragment mapping.
const ExtensionID = 1

// DecodeFragmentExtension extracts fragment index and payload offset.
func DecodeFragmentExtension(p *rtp.Packet) (fragment int, offset uint32, err error) {
	ext := p.GetExtension(ExtensionID)
	if len(ext) < 6 {
		return 0, 0, errors.New("missing fragment mapping extension")
	}
	fragment = int(binary.BigEndian.Uint16(ext[0:2]))
	offset = binary.BigEndian.Uint32(ext[2:6])
	return fragment, offset, nil
}

// recordHeader is the fixed prefix of every .rtpr frame:
// magic(4) version(1) repLen(1) frag(2) arrival(4) => 12 bytes, then repID and
// the raw RTP packet (length inferred until the next frame / EOF).
type recordHeader struct {
	repLen  byte
	frag    uint16
	arrival uint32
}

// ReadRecord parses a framed .rtpr stream. The framing is intentionally local
// to the tool: 4-byte magic, version, 1-byte rep id length, 2-byte fragment,
// 4-byte arrival microsecond/1000, rep id bytes, then one complete RTP packet.
func ReadRecord(r io.Reader) ([]RecordPacket, error) {
	lr := io.LimitReader(r, MaxRecordBytes+1)
	var out []RecordPacket
	buf := make([]byte, headerSize)
	for {
		if _, err := io.ReadFull(lr, buf[:4]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
					"truncated record magic", core.Source{})
			}
			return nil, err
		}
		if string(buf[:4]) != rtprMagic {
			return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
				"bad RTP record magic", core.Source{})
		}
		if _, err := io.ReadFull(lr, buf[4:headerSize]); err != nil {
			return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
				"truncated record header", core.Source{})
		}
		if buf[4] != rtprVersion {
			return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
				fmt.Sprintf("unsupported record version %d", buf[4]), core.Source{})
		}
		h := recordHeader{
			repLen:  buf[5],
			frag:    binary.BigEndian.Uint16(buf[6:8]),
			arrival: binary.BigEndian.Uint32(buf[8:12]),
		}
		rep := make([]byte, h.repLen)
		if _, err := io.ReadFull(lr, rep); err != nil {
			return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
				"truncated rep id", core.Source{})
		}
		// RTP packets are length-prefixed at record time (2 bytes), keeping the
		// framing independent of RTP internals.
		var lenBuf [2]byte
		if _, err := io.ReadFull(lr, lenBuf[:]); err != nil {
			return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
				"truncated packet length", core.Source{})
		}
		pktLen := int(binary.BigEndian.Uint16(lenBuf[:]))
		if pktLen == 0 || pktLen > 65535 {
			return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
				fmt.Sprintf("implausible packet length %d", pktLen), core.Source{})
		}
		raw := make([]byte, pktLen)
		if _, err := io.ReadFull(lr, raw); err != nil {
			return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
				"truncated RTP packet", core.Source{
					RepID: string(rep), SegmentIndex: int(h.frag)})
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			return nil, core.NewFault(core.FaultInvalidNAL, core.SevDamaged,
				"unparseable RTP header: "+err.Error(),
				core.Source{RepID: string(rep), SegmentIndex: int(h.frag)})
		}
		frag, offset, extErr := DecodeFragmentExtension(pkt)
		if extErr != nil {
			return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
				extErr.Error(),
				core.Source{RepID: string(rep), PacketSeq: pkt.SequenceNumber})
		}
		out = append(out, RecordPacket{
			ArrivalUS: int64(h.arrival) * 1000,
			RepID:     string(rep),
			Fragment:  frag,
			Offset:    offset,
			Packet:    pkt,
		})
		if len(out) > MaxPackets {
			return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
				"packet count exceeds limit", core.Source{})
		}
	}
	if int64(len(out)) == 0 {
		return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
			"empty RTP record", core.Source{})
	}
	return out, nil
}
