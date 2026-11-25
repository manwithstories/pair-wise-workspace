package pipeline

import (
	"bytes"

	"mediad/internal/timeline"
	"mediad/internal/transport"
)

// rtpFlow packetizes one AU, exercises pion/rtp-backed reorder/dedup, and
// verifies that reassembly restores the exact bytes.
type rtpFlow struct {
	receiver         *transport.Receiver
	seq              uint16
	RTPDuplicates    int64
	BackpressureHits int64
}

func newRTPFlow() *rtpFlow {
	return &rtpFlow{receiver: transport.NewReceiver(transport.Config{})}
}

func (f *rtpFlow) sendAU(timestamp uint32, au []byte, nowNS int64, injectDuplicate bool) error {
	packets := transport.PacketizeAU(timestamp, au, 1200, f.seq, 0x4d4544)
	f.seq += uint16(len(packets))
	for i := range packets {
		p := packets[i]
		dup, err := f.receiver.Push(&p, nowNS)
		if err != nil {
			return timeline.Wrap(timeline.KindInternal, "pipeline.rtp_push", -1, -1, err)
		}
		if dup {
			f.RTPDuplicates++
		}
		if injectDuplicate && i == 0 {
			dup, err = f.receiver.Push(&p, nowNS)
			if err != nil {
				return timeline.Wrap(timeline.KindInternal, "pipeline.rtp_duplicate_push", -1, -1, err)
			}
			if dup {
				f.RTPDuplicates++
			}
		}
	}
	aus := f.receiver.Poll(nowNS)
	if len(aus) != 1 {
		return timeline.New(timeline.KindInternal, "pipeline.rtp_reassembly", -1, -1,
			"expected one reassembled AU, got %d", len(aus))
	}
	if !aus[0].Complete || !bytes.Equal(aus[0].Data, au) {
		return timeline.New(timeline.KindUnrecoverable, "pipeline.rtp_reassembly", -1, -1,
			"RTP AU corrupt or byte mismatch complete=%v missing=%v", aus[0].Complete, aus[0].Missing)
	}
	f.BackpressureHits = f.receiver.BackpressureHits
	return nil
}
