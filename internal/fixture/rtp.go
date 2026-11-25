package fixture

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/pion/rtp"
)

// RepairCopyRecord is a redundant copy of one packet carried by another, as
// RTX or an FEC block would carry. Orig is the wire sequence being repaired.
type RepairCopyRecord struct {
	Orig      uint16 `json:"orig"`
	Payload   []byte `json:"payload"`
	Timestamp uint32 `json:"timestamp"`
	Marker    bool   `json:"marker"`
}

// RTPPacketRecord is one received packet. Repair, when present, redundantly
// re-carries an earlier packet so the receiver can restore it if it was lost.
type RTPPacketRecord struct {
	SSRC        uint32            `json:"ssrc"`
	Camera      string            `json:"camera"`
	Seq         uint16            `json:"seq"`
	Timestamp   uint32            `json:"timestamp"`
	Marker      bool              `json:"marker"`
	PayloadType uint8             `json:"payload_type"`
	Payload     []byte            `json:"payload"`
	Repair      *RepairCopyRecord `json:"repair,omitempty"`
}

// RTPTrace is a recorded, possibly lossy packet stream for one SSRC, along
// with the sender's ground truth needed to score recovery.
type RTPTrace struct {
	SSRC        uint32            `json:"ssrc"`
	Camera      string            `json:"camera"`
	Received    []RTPPacketRecord `json:"received"`    // packets that arrived
	SentSeqs    []uint16          `json:"sent_seqs"`   // every sequence the sender emitted
	LostSeqs    []uint16          `json:"lost_seqs"`   // dropped in transit
	Recoverable []uint16          `json:"recoverable"` // dropped but recoverable via redundancy
}

// RedundantFor returns the repair copies keyed by carrier wire sequence, in
// the shape recovery.Recover expects.
func (t *RTPTrace) RedundantFor() map[uint16]RepairCopyRecord {
	out := make(map[uint16]RepairCopyRecord)
	for _, r := range t.Received {
		if r.Repair != nil {
			out[r.Seq] = *r.Repair
		}
	}
	return out
}

// writeRTPFixture emits one RTP trace per camera. In the recover profile a
// deterministic subset of packets is dropped and every third packet carries a
// redundant copy of its predecessor, so single losses are recoverable.
func writeRTPFixture(opts Options, cams []Camera, info *Info, mediaDir string) error {
	rtpDir := filepath.Join(mediaDir, "rtp")
	if err := os.MkdirAll(rtpDir, 0o755); err != nil {
		return err
	}
	totalLost, totalRecoverable, totalSent := 0, 0, 0
	for ci, cam := range cams {
		rng := rand.New(rand.NewSource(opts.Seed*7919 + int64(ci)))
		trace := buildRTPTrace(opts, cam, rng)
		totalLost += len(trace.LostSeqs)
		totalRecoverable += len(trace.Recoverable)
		totalSent += len(trace.SentSeqs)
		data, err := json.Marshal(trace)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(rtpDir, cam.Name+".json"), data, 0o644); err != nil {
			return err
		}
	}
	info.Expected.LostPackets = totalLost
	info.Expected.RecoveredPackets = totalRecoverable
	info.Expected.RTPFrames = totalSent
	return nil
}

// buildRTPTrace synthesizes the sender stream and the received subset.
func buildRTPTrace(opts Options, cam Camera, rng *rand.Rand) *RTPTrace {
	const ssrcBase = 0x5EED0000
	trace := &RTPTrace{
		SSRC:   ssrcBase + uint32(len(cam.Name)+cam.Width),
		Camera: cam.Name,
	}
	framesPerCam := opts.Segments * opts.FramesPerFrag

	// Cameras with an even index start near a 16-bit sequence wrap so the
	// recovery unwrap logic is exercised.
	seqBase := uint16(100)
	if len(cam.Name)%2 == 0 {
		seqBase = 0xFFF0
	}

	// Frame payloads are keyed by frame index so a redundant copy can repeat
	// the exact bytes of its predecessor.
	type sentPacket struct {
		wire    uint16
		ts      uint32
		marker  bool
		payload []byte
	}
	sent := make([]sentPacket, 0, framesPerCam)
	for f := 0; f < framesPerCam; f++ {
		sent = append(sent, sentPacket{
			wire:    seqBase + uint16(f),
			ts:      uint32((90000 + int64(f)*3000) % (1 << 32)),
			marker:  (f+1)%opts.FramesPerFrag == 0,
			payload: samplePayload(cam, f/opts.FramesPerFrag, f%opts.FramesPerFrag),
		})
		trace.SentSeqs = append(trace.SentSeqs, sent[f].wire)
	}

	// Decide which packets are dropped in transit.
	drop := make(map[uint16]bool)
	for f := 0; f < framesPerCam; f++ {
		if opts.Profile == ProfileRecover && opts.Loss > 0 && rng.Float64() < opts.Loss {
			drop[sent[f].wire] = true
			trace.LostSeqs = append(trace.LostSeqs, sent[f].wire)
		}
	}

	// Every packet except the first redundantly re-carries the previous
	// packet's frame, emulating RTX/FEC. A dropped packet is recoverable iff
	// its successor (the carrier) survived.
	repaired := make(map[uint16]bool)
	for f := 1; f < framesPerCam; f++ {
		if drop[sent[f].wire] {
			continue // carrier lost
		}
		orig := sent[f-1].wire
		if drop[orig] && !repaired[orig] {
			repaired[orig] = true
			trace.Recoverable = append(trace.Recoverable, orig)
		}
	}

	for f := 0; f < framesPerCam; f++ {
		if drop[sent[f].wire] {
			continue
		}
		rec := RTPPacketRecord{
			SSRC:        trace.SSRC,
			Camera:      cam.Name,
			Seq:         sent[f].wire,
			Timestamp:   sent[f].ts,
			Marker:      sent[f].marker,
			PayloadType: 96,
			Payload:     sent[f].payload,
		}
		if f > 0 {
			rec.Repair = &RepairCopyRecord{
				Orig:      sent[f-1].wire,
				Payload:   sent[f-1].payload,
				Timestamp: sent[f-1].ts,
				Marker:    sent[f-1].marker,
			}
		}
		trace.Received = append(trace.Received, rec)
	}
	// Deterministic shuffle of wire order to exercise reordering.
	if len(trace.Received) > 1 {
		sh := rand.New(rand.NewSource(opts.Seed + int64(len(cam.Name))))
		for i := len(trace.Received) - 1; i > 0; i-- {
			j := sh.Intn(i + 1)
			trace.Received[i], trace.Received[j] = trace.Received[j], trace.Received[i]
		}
	}
	// Inject one duplicate per camera to exercise duplicate detection.
	if len(trace.Received) > 2 {
		dup := trace.Received[len(trace.Received)/2]
		trace.Received = append(trace.Received, dup)
	}
	return trace
}

// ToRTP converts a fixture record into a pion/rtp packet so the recovery path
// is validated against a real RTP parser rather than a bespoke decoder.
func ToRTP(rec RTPPacketRecord) (*rtp.Packet, error) {
	if len(rec.Payload) > 1400 {
		return nil, fmt.Errorf("fixture: payload too large for one RTP packet")
	}
	p := &rtp.Packet{Header: rtp.Header{
		Version:        2,
		Marker:         rec.Marker,
		PayloadType:    rec.PayloadType,
		SequenceNumber: rec.Seq,
		Timestamp:      rec.Timestamp,
		SSRC:           rec.SSRC,
	}}
	p.Payload = rec.Payload
	return p, nil
}

// RTPSamplePayload exposes the deterministic payload used by fixtures.
func RTPSamplePayload(cam Camera, seg, frame int) []byte { return samplePayload(cam, seg, frame) }
