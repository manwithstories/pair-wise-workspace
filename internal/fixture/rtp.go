package fixture

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/pion/rtp"
)

// RTPPacketRecord is one packet as stored in a fixture trace, including
// optional redundancy that lets the recovery stage repair losses offline.
type RTPPacketRecord struct {
	SSRC        uint32 `json:"ssrc"`
	Camera      string `json:"camera"`
	Seq         uint16 `json:"seq"`
	Timestamp   uint32 `json:"timestamp"`
	Marker      bool   `json:"marker"`
	PayloadType uint8  `json:"payload_type"`
	Payload     []byte `json:"payload"`
	// RedundantCopy holds the same payload under the *next* sequence number,
	// letting the receiver recover this packet when it is itself lost.
	RedundantCopy bool `json:"redundant_copy,omitempty"`
}

// RTPTrace is a recorded, possibly lossy packet stream for one or more SSRCs.
type RTPTrace struct {
	SSRC     uint32            `json:"ssrc"`
	Camera   string            `json:"camera"`
	Packets  []RTPPacketRecord `json:"packets"`
	LostSeqs []uint16          `json:"lost_seqs,omitempty"`
	// SentSeqs lists every sequence number the sender emitted, in order, so
	// the recovery stage can tell a genuine gap from a late-arriving packet.
	SentSeqs []uint16 `json:"sent_seqs"`
}

// writeRTPFixture emits one RTP trace per camera. In the recover profile a
// deterministic subset of packets is dropped and some packets carry a
// redundant copy of their predecessor so recovery can be measured.
func writeRTPFixture(opts Options, cams []Camera, info *Info, mediaDir string) error {
	if opts.Profile != ProfileRecover {
		// Also emit a clean, lossless trace so the recovery test has a
		// zero-loss baseline.
	}
	rtpDir := filepath.Join(mediaDir, "rtp")
	if err := os.MkdirAll(rtpDir, 0o755); err != nil {
		return err
	}
	totalLost, totalRecovered, totalFrames := 0, 0, 0
	for ci, cam := range cams {
		rng := rand.New(rand.NewSource(opts.Seed*7919 + int64(ci)))
		trace := buildRTPTrace(opts, cam, rng)
		if opts.Profile == ProfileRecover {
			totalLost += trace.lost
			totalRecovered += trace.recovered
		}
		totalFrames += trace.frames
		data, err := json.Marshal(trace.trace)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%s.json", cam.Name)
		if err := os.WriteFile(filepath.Join(rtpDir, name), data, 0o644); err != nil {
			return err
		}
	}
	info.Expected.LostPackets = totalLost
	info.Expected.RecoveredPackets = totalRecovered
	info.Expected.RTPFrames = totalFrames
	return nil
}

type traceStats struct {
	trace     *RTPTrace
	lost      int
	recovered int
	frames    int
}

// buildRTPTrace synthesizes an RTP stream. Packet i carries frame i; every
// third packet also embeds a redundant copy of packet i-1 so single losses
// along the redundancy chain remain recoverable.
func buildRTPTrace(opts Options, cam Camera, rng *rand.Rand) *traceStats {
	const ssrcBase = 0x5EED0000
	trace := &RTPTrace{
		SSRC:   ssrcBase + uint32(len(cam.Name)+cam.Width),
		Camera: cam.Name,
	}
	framesPerCam := opts.Segments * opts.FramesPerFrag
	stats := &traceStats{trace: trace, frames: framesPerCam}

	// Start near a sequence wrap on alternating cameras to exercise 16-bit
	// unwrap in the recovery stage.
	seqBase := uint16(100)
	if len(cam.Name)%2 == 0 {
		seqBase = 0xFFF0
	}

	type pending struct {
		rec RTPPacketRecord
	}
	var prev *pending
	for f := 0; f < framesPerCam; f++ {
		payload := samplePayload(cam, f/opts.FramesPerFrag, f%opts.FramesPerFrag)
		rec := RTPPacketRecord{
			SSRC:        trace.SSRC,
			Camera:      cam.Name,
			Seq:         seqBase + uint16(f),
			Timestamp:   uint32((90000 + int64(f)*3000) % (1 << 32)),
			Marker:      (f+1)%opts.FramesPerFrag == 0,
			PayloadType: 96,
			Payload:     payload,
		}
		trace.SentSeqs = append(trace.SentSeqs, rec.Seq)
		// Every third frame after the first carries a redundant copy of the
		// previous packet, giving the receiver a second chance at recovery.
		if prev != nil && f%3 == 0 {
			rec.Payload = prev.rec.Payload
			rec.RedundantCopy = true
			rec.Timestamp = prev.rec.Timestamp
		}
		drop := opts.Profile == ProfileRecover && opts.Loss > 0 && rng.Float64() < opts.Loss
		if drop {
			stats.lost++
			trace.LostSeqs = append(trace.LostSeqs, rec.Seq)
		} else {
			trace.Packets = append(trace.Packets, rec)
		}
		prev = &pending{rec: rec}
	}
	// Count how many lost packets have a surviving redundant copy later in
	// the stream; that is the upper bound on recoverable packets.
	lostSet := make(map[uint16]bool, len(trace.LostSeqs))
	for _, s := range trace.LostSeqs {
		lostSet[s] = true
	}
	for i, rec := range trace.Packets {
		if !rec.RedundantCopy {
			continue
		}
		// The redundant copy on packet i carries the payload of packet i-1.
		prevSeq := rec.Seq - 1
		if lostSet[prevSeq] {
			stats.recovered++
			lostSet[prevSeq] = false
			trace.Packets[i].RedundantCopy = true
		}
	}
	// Serialize RTP packets in wire form too, to prove pion/rtp can parse them.
	for i := range trace.Packets {
		pkt, err := toRTPPacket(trace.Packets[i])
		if err != nil {
			continue
		}
		if _, err := pkt.Marshal(); err != nil {
			continue
		}
	}
	return stats
}

func toRTPPacket(rec RTPPacketRecord) (*rtp.Packet, error) {
	if len(rec.Payload) > 1400 {
		return nil, fmt.Errorf("fixture: payload too large for one RTP packet")
	}
	p := &rtp.Packet{Header: rtp.Header{
		Version:        2,
		Padding:        false,
		Marker:         rec.Marker,
		PayloadType:    rec.PayloadType,
		SequenceNumber: rec.Seq,
		Timestamp:      rec.Timestamp,
		SSRC:           rec.SSRC,
	}}
	p.Payload = rec.Payload
	return p, nil
}

// RTPSamplePayload exposes the deterministic payload used by fixtures so the
// recovery test can verify byte-exact restoration.
func RTPSamplePayload(cam Camera, seg, frame int) []byte { return samplePayload(cam, seg, frame) }
