// Package recovery turns lossy RTP capture logs into ordered media samples:
// it removes duplicates, reorders by sequence number, reconstructs single
// packet losses from XOR FEC groups and marks any multi-packet loss as a gap
// instead of inventing data.
package recovery

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/pion/rtp"

	"relay/internal/clock"
	"relay/internal/demux"
	"relay/internal/media"
)

// RepairPayloadType marks an RTP packet as an XOR FEC repair packet rather
// than a media packet.
const RepairPayloadType = 127

// repairHdrSize is baseSeq(u16) + memberCount(u8).
const repairHdrSize = 3

// Gap records an unrecovered hole in a media packet sequence.
type Gap struct {
	SourceID      uint32
	FirstSequence uint64
	LastSequence  uint64
}

// Result is the outcome of recovering one capture log.
type Result struct {
	Frames       []*media.RawFrame // ordered, deduplicated media samples
	Duplicates   int
	Recovered    int
	Gaps         []Gap
	MediaCount   int // media packets present after recovery
	ExpectedSpan int // sequence span from first to last media packet
}

// EncodeRepair builds the repair payload for one FEC group: a 3-byte header
// followed by the XOR of all member payloads (shorter payloads zero-padded).
func EncodeRepair(baseSeq uint16, members [][]byte) []byte {
	maxLen := 0
	for _, m := range members {
		if len(m) > maxLen {
			maxLen = len(m)
		}
	}
	out := make([]byte, repairHdrSize+maxLen)
	binary.BigEndian.PutUint16(out[0:2], baseSeq)
	out[2] = byte(len(members))
	for _, m := range members {
		for i, b := range m {
			out[repairHdrSize+i] ^= b
		}
	}
	return out
}

// entry is one indexed media packet plus its provenance.
type entry struct {
	pkt       *rtp.Packet
	recovered bool
}

// Recover processes one parsed RTP log. Media packets are keyed by their
// unwrapped 16-bit sequence number; repair packets are applied once all media
// is indexed so arrival order of repair traffic does not matter.
func Recover(in []demux.RTPPacket, hdr demux.RTPLogHeader) (*Result, error) {
	res := &Result{}
	mediaBySeq := map[uint64]*entry{}
	repairs := []*rtp.Packet{}
	var seqU clock.SeqUnwrapper
	var minSeq, maxSeq uint64
	haveRange := false
	mediaPT := uint8(0)

	for _, rec := range in {
		p := rec.Packet
		if p.PayloadType == RepairPayloadType {
			repairs = append(repairs, p)
			continue
		}
		seq := seqU.Next(p.SequenceNumber)
		if existing, ok := mediaBySeq[seq]; ok {
			// Duplicate: keep the first occurrence, count every repeat.
			if existing != nil {
				res.Duplicates++
			}
			continue
		}
		mediaBySeq[seq] = &entry{pkt: p}
		mediaPT = p.PayloadType // all media packets in one log share a PT
		if !haveRange {
			minSeq, maxSeq, haveRange = seq, seq, true
		} else {
			if seq < minSeq {
				minSeq = seq
			}
			if seq > maxSeq {
				maxSeq = seq
			}
		}
	}

	// Apply XOR repair groups while at most one member is missing.
	for _, rp := range repairs {
		base, count, xored, err := decodeRepair(rp.Payload)
		if err != nil {
			return nil, media.NewError(media.ErrMetadata, hdr.SourceID, "fec: "+err.Error())
		}
		baseUnwrapped := nearestUnwrapped(mediaBySeq, minSeq, uint64(base), haveRange)
		missing := []uint64{}
		for i := uint8(0); i < count; i++ {
			if _, ok := mediaBySeq[baseUnwrapped+uint64(i)]; !ok {
				missing = append(missing, baseUnwrapped+uint64(i))
			}
		}
		if len(missing) == 0 {
			continue // redundant repair, nothing to do
		}
		if len(missing) > 1 {
			continue // multi-packet loss: handled as a gap below
		}
		payload := append([]byte(nil), xored...)
		for i := uint8(0); i < count; i++ {
			seq := baseUnwrapped + uint64(i)
			if seq == missing[0] {
				continue
			}
			mp := mediaBySeq[seq].pkt.Payload
			for j := range payload {
				if j < len(mp) {
					payload[j] ^= mp[j]
				}
			}
		}
		recovered := rebuildPacket(rp, mediaPT, uint16(missing[0]), payload)
		mediaBySeq[missing[0]] = &entry{pkt: recovered, recovered: true}
		res.Recovered++
		if missing[0] < minSeq || !haveRange {
			minSeq = missing[0]
		}
		if missing[0] > maxSeq {
			maxSeq = missing[0]
		}
	}

	// Mark every remaining hole as a gap; gaps are never silently filled.
	if haveRange {
		for seq := minSeq; seq <= maxSeq; seq++ {
			if _, ok := mediaBySeq[seq]; ok {
				continue
			}
			first := seq
			last := seq
			for last+1 <= maxSeq {
				if _, ok := mediaBySeq[last+1]; ok {
					break
				}
				last++
			}
			res.Gaps = append(res.Gaps, Gap{
				SourceID: hdr.SourceID, FirstSequence: first, LastSequence: last,
			})
			seq = last
		}
	}

	seqs := make([]uint64, 0, len(mediaBySeq))
	for seq := range mediaBySeq {
		seqs = append(seqs, seq)
	}
	sortU64(seqs)
	var tsU clock.Unwrapper
	for _, seq := range seqs {
		p := mediaBySeq[seq].pkt
		ticks := tsU.Next(p.Timestamp)
		payload := append([]byte(nil), p.Payload...)
		sum := hashPayload(payload)
		fr := &media.RawFrame{
			Kind: hdr.Kind,
			Meta: media.FrameMeta{
				SourceID:     hdr.SourceID,
				Sequence:     seq,
				ClockTicks:   ticks,
				TimescaleHz:  uint64(hdr.ClockRateHz),
				CaptureRefNS: captureRefNS(p),
				Width:        hdr.Width,
				Height:       hdr.Height,
				Size:         uint32(len(payload)),
				SHA256:       sum,
			},
			Payload: payload,
		}
		res.Frames = append(res.Frames, fr)
	}
	res.MediaCount = len(mediaBySeq)
	if haveRange {
		res.ExpectedSpan = int(maxSeq-minSeq) + 1
	}
	return res, nil
}

func decodeRepair(b []byte) (baseSeq uint16, count uint8, xored []byte, err error) {
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
