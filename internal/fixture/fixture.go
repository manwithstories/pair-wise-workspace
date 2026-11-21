// Package fixture generates the self-contained synthetic media set used by the
// demo: MXF/MP4 metadata with rotation/crop/color conflicts, and RTP streams
// subject to duplication, reordering, burst loss and bandwidth cliff schedules.
package fixture

import (
	"math/rand"

	"github.com/pion/rtp"

	"mediasync/internal/metadata"
	"mediasync/internal/model"
	"mediasync/internal/transport"
)

// Config controls one deterministic fixture generation run.
type Config struct {
	Seed           int64
	DurationTicks  int64 // shared 1kHz simulation clock
	LossBurstTicks int64 // length of the injected loss burst
	VideoRate      model.Rational
	Master         model.Clock
	Reps           []transport.Representation
	// BandwidthSchedule maps a 1kHz tick to an available bps ceiling.
	BandwidthSchedule func(tick int64) int64
}

// MetadataSet is the generated container metadata for one video + one audio track.
type MetadataSet struct {
	MXFFixture []byte
	MP4Fixture []byte
	MXFResult  *metadata.ParseResult
	MP4Result  *metadata.ParseResult
}

// BuildMetadata writes conflicting MXF/MP4 records for the same logical tracks.
func BuildMetadata(cfg Config) (MetadataSet, error) {
	mxfTracks := []metadata.TrackMeta{
		{TrackID: "cam1", Kind: model.KindVideo, Source: "mxf", Timescale: 90000,
			Start: model.Rational{Num: 0, Den: 1}, FrameRate: cfg.VideoRate,
			Width: 1920, Height: 1080, Rotation: 180, HasRot: true,
			Crop:  &metadata.CropRect{X: 0, Y: 0, Width: 1920, Height: 1080},
			Color: &metadata.ColorDesc{ColorType: "nclx", Primaries: 9, Transfer: 16, Matrix: 9}},
		{TrackID: "mic1", Kind: model.KindAudio, Source: "mxf", Timescale: 48000,
			Start: model.Rational{Num: 0, Den: 1}, FrameRate: model.Rational{Num: 48000, Den: 1}},
	}
	mp4Tracks := []metadata.TrackMeta{
		{TrackID: "cam1", Kind: model.KindVideo, Source: "mp4", Timescale: 90000,
			FrameRate: cfg.VideoRate, Width: 1920, Height: 1080, Rotation: 90, HasRot: true,
			Crop:  &metadata.CropRect{X: 10, Y: 10, Width: 1900, Height: 1060},
			Color: &metadata.ColorDesc{ColorType: "nclx", Primaries: 1, Transfer: 1, Matrix: 1}},
		{TrackID: "mic1", Kind: model.KindAudio, Source: "mp4", Timescale: 48000,
			FrameRate: model.Rational{Num: 48000, Den: 1}},
	}
	var set MetadataSet
	mxfBuf := &byteBuffer{}
	if err := metadata.WriteMXF(mxfBuf, mxfTracks); err != nil {
		return set, err
	}
	set.MXFFixture = mxfBuf.bytes()
	var err error
	set.MXFResult, err = metadata.ParseMXF(byteReader(set.MXFFixture), "synthetic.mxf")
	if err != nil {
		return set, err
	}
	mp4Buf := &byteBuffer{}
	if err := metadata.WriteMP4(mp4Buf, mp4Tracks); err != nil {
		return set, err
	}
	set.MP4Fixture = mp4Buf.bytes()
	set.MP4Result, err = metadata.ParseMP4(byteReader(set.MP4Fixture), "synthetic.mp4")
	return set, err
}

// StreamEvent is one generated network event: a packet, or an injected loss.
type StreamEvent struct {
	Packet  transport.SimPacket
	Dropped bool
	IsAudio bool
}

// retransItem remembers a lost frame's index, original RTP sequence and the
// representation that owned it, so retransmission fills the correct tier's hole.
type retransItem struct {
	frame int
	seq   uint16
	rep   int
}

// GenerateVideoStream builds one RTP packet per video frame. At any shared tick
// exactly one representation is the active contributor (ABR semantics), so no
// two reps can contribute the same frame. Representation changes land on a frame
// boundary and keep a continuous RTP timestamp. A loss burst hits the active
// representation; the following frames retransmit the lost sequence numbers
// inside the reorder window so most burst loss is recoverable.
func GenerateVideoStream(rng *rand.Rand, cfg Config) ([]StreamEvent, int) {
	frameStep := cfg.Master.TickRate * cfg.VideoRate.Den / cfg.VideoRate.Num
	nFrames := int(cfg.DurationTicks * cfg.VideoRate.Num / cfg.VideoRate.Den / 1000)
	var events []StreamEvent
	dropped := 0
	// Place the burst in the high-bitrate first segment so it hits the highest
	// representation while the stream is at its top tier.
	burstStartFrame := nFrames / 6
	// Convert the millisecond burst length to a whole frame count using the exact
	// frame duration, so an 80ms flag yields a real ~80ms burst of lost frames.
	frameDurationMs := cfg.DurationTicks / int64(nFrames)
	burstFrames := int((cfg.LossBurstTicks + frameDurationMs/2) / frameDurationMs)
	if burstFrames < 1 {
		burstFrames = 1
	}
	burstEndFrame := burstStartFrame + burstFrames
	repNextSeq := make([]uint16, len(cfg.Reps)) // next ORIGINAL sequence per rep
	var pendingRetrans []retransItem            // lost frames awaiting in-window retry
	const retransWindow = 18                    // inside the receiver reorder window
	activeRep := 0
	for i := 0; i < nFrames; i++ {
		sendTick := int64(i) * cfg.DurationTicks / int64(nFrames)
		ts := uint32(int64(i) * frameStep)

		inBurst := i >= burstStartFrame && i < burstEndFrame
		// Representation selection follows the bandwidth schedule on frame
		// boundaries, but stays pinned for the whole loss burst so sequence
		// accounting cannot interleave two representations mid-burst.
		wantRep := selectRep(cfg, sendTick)
		if !inBurst && wantRep != activeRep {
			// A switch is an exclusive boundary: pending retransmissions from the
			// old tier cannot re-enter on the new tier's timeline (they would make
			// two reps contribute the same tick). They become unrecoverable gaps.
			pendingRetrans = nil
			activeRep = wantRep
		}

		// Retransmit lost frames using their ORIGINAL sequence number (real RTP
		// retransmission semantics), so the receiver fills the hole and counts
		// recovery. Each item is sent once and removed; items past the reorder
		// window are dropped entirely and stay unrecoverable gaps.
		var pending []retransItem
		for _, item := range pendingRetrans {
			age := i - item.frame
			if age > retransWindow {
				continue // unrecoverable: do not retransmit
			}
			if age <= 1 {
				pending = append(pending, item) // wait at least one frame
				continue
			}
			events = append(events, StreamEvent{Packet: transport.SimPacket{
				RepIndex: item.rep, Seq: item.seq,
				RTPTS: uint32(int64(item.frame) * frameStep), Marker: true,
				Payload:   framePayload(item.frame, item.rep),
				SendTicks: sendTick, RecvTicks: sendTick + 2,
			}})
		}
		pendingRetrans = pending

		recvTick := sendTick + 1 + rng.Int63n(4)
		origSeq := repNextSeq[activeRep]
		ev := StreamEvent{Packet: transport.SimPacket{
			RepIndex: activeRep, Seq: origSeq, RTPTS: ts, Marker: true,
			Payload: framePayload(i, activeRep), SendTicks: sendTick, RecvTicks: recvTick,
		}}
		repNextSeq[activeRep]++ // original sequence advances exactly once per frame
		if inBurst {
			// The burst drops the current frame; it is queued for retransmission.
			ev.Dropped = true
			dropped++
			pendingRetrans = append(pendingRetrans, retransItem{frame: i, seq: origSeq, rep: activeRep})
		}
		events = append(events, ev)
	}
	return events, dropped
}

// selectRep maps the bandwidth ceiling to the highest affordable representation.
func selectRep(cfg Config, tick int64) int {
	if cfg.BandwidthSchedule == nil {
		return len(cfg.Reps) - 1
	}
	bps := cfg.BandwidthSchedule(tick)
	chosen := 0
	for i, r := range cfg.Reps {
		if r.Bitrate <= bps {
			chosen = i
		}
	}
	return chosen
}

// Perturb applies duplicate injection and reordering deterministically.
func Perturb(rng *rand.Rand, events []StreamEvent, duplicateEvery, reorderWindow int) []StreamEvent {
	var out []StreamEvent
	var hold []StreamEvent
	for i, ev := range events {
		if !ev.Dropped && duplicateEvery > 0 && i%duplicateEvery == 7 {
			out = append(out, ev) // duplicate delivered twice
		}
		hold = append(hold, ev)
		if len(hold) >= reorderWindow {
			j := rng.Intn(len(hold))
			hold[0], hold[j] = hold[j], hold[0]
			out = append(out, hold[0])
			hold = hold[1:]
		}
	}
	out = append(out, hold...)
	return out
}

// RTPPacket converts a SimPacket to a pion RTP packet for external verification.
func RTPPacket(p transport.SimPacket) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96,
		SequenceNumber: p.Seq, Timestamp: p.RTPTS, Marker: p.Marker},
		Payload: append([]byte(nil), p.Payload...)}
}

func framePayload(i, rep int) []byte {
	return []byte{byte(i >> 8), byte(i), byte(rep), 0xA5}
}
