package pipeline

import (
	"fmt"

	"mediasim/internal/mp4"
	"mediasim/internal/rtp"
)

// transportResult is the receiver's view of the simulated network.
type transportResult struct {
	RTP       RTPReport
	Delivered []int // output frame indexes, ascending
	LatencyMs map[int]float64
	repaired  map[int]struct{}
	Errors    []error
}

// simulateTransport encodes the frame packets, applies deterministic burst
// loss, duplication, adjacent swaps and jitter, drains a bounded jitter
// buffer, then repairs missing frames from the local origin (the demuxed
// container). A frame whose bytes are truncated or corrupt cannot be
// repaired and stays missing, which makes the run partial rather than wrong.
func simulateTransport(cfg Config, info *mp4.Info, rec *mp4.Recovery) transportResult {
	out := transportResult{LatencyMs: map[int]float64{}, repaired: map[int]struct{}{}}
	packets := transportPlan()
	for i := range packets {
		if err := packets[i].Encode(0x5eed); err != nil {
			out.Errors = append(out.Errors, err)
			return out
		}
	}
	survivors, lostFrames := applyBurstLoss(packets, cfg.LossBurst, cfg.BurstEvery)
	reord, err := rtp.NewReorderer(rtp.ReorderOptions{DuplicateEvery: 23, SwapEvery: 11})
	if err != nil {
		out.Errors = append(out.Errors, err)
		return out
	}
	arrivals := reord.Apply(survivors)
	jb, err := rtp.NewJitterBuffer(rtp.JitterOptions{
		TargetDelayNanos: int64(cfg.JitterMs) * 1e6,
		MaxPackets:       cfg.MaxPackets,
	})
	if err != nil {
		out.Errors = append(out.Errors, err)
		return out
	}
	var played []rtp.Packet
	for _, p := range arrivals {
		rel, errs := jb.Insert(p, playoutStamp(p.Frame, p.Seq, cfg))
		out.Errors = append(out.Errors, errs...)
		played = append(played, rel...)
	}
	played = append(played, jb.Flush()...)

	delivered := map[int]struct{}{}
	for _, p := range played {
		delivered[p.Frame] = struct{}{}
	}
	// Frames the network failed to deliver, measured before repair.
	networkLost := make([]int, 0, len(lostFrames))
	lostSet := map[int]struct{}{}
	for _, f := range lostFrames {
		if _, ok := delivered[f]; !ok {
			lostSet[f] = struct{}{}
			networkLost = append(networkLost, f)
		}
	}
	// Repair pass: ask the local origin for the frames the network withheld.
	repaired, unrepaired := 0, 0
	for _, frame := range networkLost {
		if localFrameAvailable(info, rec, frame) {
			delivered[frame] = struct{}{}
			out.repaired[frame] = struct{}{}
			repaired++
			continue
		}
		unrepaired++
		out.Errors = append(out.Errors, fmt.Errorf(
			"transport: frame %d lost in transit and unavailable from the local origin", frame))
	}
	for _, frame := range sortedFramesOf(delivered) {
		out.Delivered = append(out.Delivered, frame)
		out.LatencyMs[frame] = frameLatencyMs(frame, cfg)
	}
	jst := jb.Stats()
	out.RTP = RTPReport{
		Sent: len(packets), Received: len(arrivals),
		Duplicate: countDuplicates(arrivals), Reordered: countReordered(arrivals),
		Late: jst.Dropped, Delivered: len(out.Delivered), MaxBuffered: jst.MaxDepth,
		KeyframeWaits: jst.KeyframeWaits, Expired: jst.Expired,
		Repaired: repaired, Unrepaired: unrepaired,
	}
	// Lost is network loss before repair, so repaired+unrepaired == lost.
	out.RTP.Lost = len(lostSet)
	return out
}

// applyBurstLoss drops deterministic bursts and returns the lost frames.
func applyBurstLoss(in []rtp.Packet, burst, every int) (kept []rtp.Packet, lost []int) {
	if burst <= 0 || every <= 0 || burst >= every {
		return in, nil
	}
	last := len(in) - every // never drop inside the final window
	for i, p := range in {
		window := i / every
		if window%3 == 1 && i%every < burst && i < last {
			lost = append(lost, p.Frame)
			continue
		}
		kept = append(kept, p)
	}
	return kept, lost
}
