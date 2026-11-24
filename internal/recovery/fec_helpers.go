package recovery

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/pion/rtp"

	"relay/internal/media"
)

// DecodeRepair parses an XOR repair payload into its group base sequence,
// member count and zero-padded parity bytes.
func DecodeRepair(b []byte) (baseSeq uint16, count uint8, xored []byte, err error) {
	if len(b) < repairHdrSize || b[2] == 0 {
		return 0, 0, nil, media.NewError(media.ErrMetadata, 0, "short repair payload")
	}
	return binary.BigEndian.Uint16(b[0:2]), b[2], b[repairHdrSize:], nil
}

// nearestUnwrapped maps a raw 16-bit group base onto the unwrapped range of
// observed media sequences, choosing the wrap that lands closest to the span.
func nearestUnwrapped(index map[uint64]*entry, observedMin uint64, raw uint64, haveRange bool) uint64 {
	if !haveRange {
		return raw
	}
	mod := uint64(1 << 16)
	k := observedMin / mod
	candidates := []uint64{k*mod + raw, (k+1)*mod + raw}
	best := candidates[0]
	bestDist := absDiff(best, observedMin)
	for _, c := range candidates[1:] {
		if d := absDiff(c, observedMin); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// rebuildPacket clones a repair packet's header into a media packet carrying
// the reconstructed payload and the missing sequence number.
func rebuildPacket(template *rtp.Packet, mediaPT uint8, seq uint16, payload []byte) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:          2,
			Marker:           true,
			PayloadType:      mediaPT,
			SequenceNumber:   seq,
			Timestamp:        template.Timestamp,
			SSRC:             template.SSRC,
			CSRC:             append([]uint32(nil), template.CSRC...),
			Extension:        template.Extension,
			ExtensionProfile: template.ExtensionProfile,
			Extensions:       append([]rtp.Extension(nil), template.Extensions...),
		},
		Payload: payload,
	}
}

// hashPayload is the SHA-256 digest convention shared with the demuxers.
func hashPayload(p []byte) [32]byte {
	return sha256.Sum256(p)
}

// CaptureRefExtProfile marks the RTP header-extension area whose first bytes
// carry the 8-byte big-endian capture wall-clock reference (a drift anchor).
// RFC 8285 one-byte framing (0x18 length nibble precedes the anchor) is used
// so pion parses the packet and the raw reader locates the anchor directly.
const CaptureRefExtProfile = 0xBEDE

func captureRefNS(p *rtp.Packet) int64 {
	if !p.Extension || p.ExtensionProfile != CaptureRefExtProfile {
		return 0
	}
	raw, err := p.Marshal()
	if err != nil {
		return 0
	}
	ext, ok := extractExtData(raw)
	if !ok || len(ext) < 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(ext[:8]))
}

// extractExtData locates the header-extension data area in a marshalled RTP
// packet, skipping the fixed header, CSRC list and 4-byte extension header.
func extractExtData(raw []byte) ([]byte, bool) {
	if len(raw) < 12 {
		return nil, false
	}
	csrcCount := int(raw[0] & 0x0f)
	pos := 12 + 4*csrcCount
	if pos+4 > len(raw) {
		return nil, false
	}
	lengthWords := int(binary.BigEndian.Uint16(raw[pos+2 : pos+4]))
	pos += 4
	end := pos + 4*lengthWords
	if end > len(raw) {
		return nil, false
	}
	return raw[pos:end], true
}

func sortU64(v []uint64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1] > v[j]; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}
