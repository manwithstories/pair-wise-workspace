package demo

import (
	"github.com/pion/rtp"

	mrtp "medialab/internal/rtp"
	"medialab/internal/sync"
)

// PacketDiagnostics summarizes the standalone fault-injection scenarios.
type PacketDiagnostics struct {
	DuplicateDropped  int
	ConflictsIsolated int
	ReorderedPackets  int
	DeliveredPackets  int
	SequenceWrapOK    bool
	TimestampWrapOK   bool
	WindowOverflowOK  bool
	ContinuousLossOK  bool
	ContinuousDropped int
}

func diagnosticPacket(seq uint16, fill byte, offset int64) *mrtp.InPacket {
	payload := make([]byte, 16)
	for i := range payload {
		payload[i] = fill ^ byte(i)
	}
	return &mrtp.InPacket{
		P:         rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: uint32(seq) * 3000}, Payload: payload},
		TrackID:   99,
		RawOffset: offset,
	}
}

func runPacketDiagnostics() PacketDiagnostics {
	d := PacketDiagnostics{}

	dup := mrtp.NewReorderBuffer(16)
	base := diagnosticPacket(1000, 0xa0, 4096)
	dup.Push(base)
	dup.Push(diagnosticPacket(1000, 0xa0, 4160))
	dup.Push(diagnosticPacket(1000, 0xa0, 4224))
	dup.Push(diagnosticPacket(1000, 0xb1, 4288))
	d.DuplicateDropped = dup.Stats.DuplicateDropped
	d.ConflictsIsolated = dup.Stats.ConflictsIsolated

	wrap := mrtp.NewReorderBuffer(16)
	order := []uint16{65530, 65533, 65532, 65531, 0, 65535}
	for i, seq := range order {
		wrap.Push(diagnosticPacket(seq, byte(seq), int64(8192+i*64)))
	}
	d.ReorderedPackets = wrap.Stats.ReorderedPackets
	d.DeliveredPackets = wrap.Stats.DeliveredPackets
	d.SequenceWrapOK = wrap.Stats.DeliveredPackets == len(order) && wrap.Stats.ReorderedPackets == 3

	u := &mrtp.TimestampUnwrapper{}
	stamps := []uint32{^uint32(0) - 1000, ^uint32(0), 0, 1000}
	first := u.Unwrap(stamps[0])
	u.Unwrap(stamps[1])
	u.Unwrap(stamps[2])
	last := u.Unwrap(stamps[3])
	d.TimestampWrapOK = last > first && last-first > 2000

	bounded := mrtp.NewReorderBuffer(4)
	bounded.Push(diagnosticPacket(10, 1, 12000))
	res := bounded.Push(diagnosticPacket(20, 2, 12064))
	d.WindowOverflowOK = res.Disposition == mrtp.DispositionOverflow && bounded.Stats.OverflowDropped == 1

	lossReport := continuousLossBoundary()
	d.ContinuousDropped = lossReport.VideoDropped
	d.ContinuousLossOK = lossReport.VideoDropped == 10 && lossReport.AVSkewMS <= sync.DriftThresholdMS
	return d
}

func continuousLossBoundary() *sync.Report {
	video := make([]sync.Frame, 12)
	audio := make([]sync.Frame, 12)
	for i := range video {
		video[i] = sync.Frame{Segment: i, Kind: "video", Present: true, Verified: false}
		audio[i] = sync.Frame{Segment: i, Kind: "audio", Present: true, Verified: true, DurationMS: 20}
	}
	for i := 0; i < 10; i++ {
		video[i].State = "damaged_dropped"
		video[i].Present = true
	}
	video[10] = sync.Frame{Segment: 10, Kind: "video", Present: true, Keyframe: true, Verified: true}
	video[11] = sync.Frame{Segment: 11, Kind: "video", Present: true, Verified: true}
	return sync.Align(video, audio)
}
