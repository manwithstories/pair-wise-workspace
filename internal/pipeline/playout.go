package pipeline

import (
	"mediasim/internal/fixture"
	"mediasim/internal/graph"
	"mediasim/internal/mp4"
	"mediasim/internal/rtp"
)

// localFrameAvailable reports whether the demuxed origin still has usable
// bytes for an output frame: the index when present, otherwise the scan.
func localFrameAvailable(info *mp4.Info, rec *mp4.Recovery, outputFrame int) bool {
	source, idx := frameSample(outputFrame)
	if info != nil && info.Indexed {
		for _, tr := range info.Tracks {
			if sourceName(tr.ID) != source {
				continue
			}
			return idx < len(tr.Samples) && tr.Samples[idx].Valid
		}
		return false
	}
	if rec == nil {
		return false
	}
	for _, b := range rec.Committing() {
		if b.Source == source && b.Index == idx {
			return true
		}
	}
	return false
}

// frameSample maps an output frame to its source camera and source index.
func frameSample(outputFrame int) (string, int) {
	if outputFrame < fixture.Cam1StartFrame {
		return fixture.SourceCam0, outputFrame
	}
	return fixture.SourceCam1, outputFrame - fixture.Cam1StartFrame
}

// sourceName maps a container track id to the fixture source name.
func sourceName(trackID uint32) string {
	switch trackID {
	case fixture.TrackCam0:
		return fixture.SourceCam0
	case fixture.TrackCam1:
		return fixture.SourceCam1
	case fixture.TrackVoice:
		return fixture.SourceVoice
	}
	return ""
}

// playoutStamp is the receiver's timestamp for an arrival. Capture time is
// the frame period; signed jitter perturbs it deterministically.
func playoutStamp(frame int, seq uint16, cfg Config) int64 {
	return int64(frame)*framePeriodNanos() + deterministicJitter(seq, cfg.Seed, cfg.JitterMax)
}

func framePeriodNanos() int64 {
	return int64(fixture.VideoFrameTicks) * 1e9 / fixture.TickNum
}

// frameLatencyMs is capture-to-commit latency: the configured jitter target,
// half a frame of queueing and this frame's own deterministic jitter. It is
// bounded by construction rather than growing with stream length.
func frameLatencyMs(frame int, cfg Config) float64 {
	j := deterministicJitter(uint16(frame), cfg.Seed, cfg.JitterMax)
	if j < 0 {
		j = -j
	}
	return float64(cfg.JitterMs) + float64(framePeriodNanos())/2e6 + float64(j)/1e6
}

func countDuplicates(in []rtp.Packet) int {
	seen := map[uint16]struct{}{}
	dup := 0
	for _, p := range in {
		if _, ok := seen[p.Seq]; ok {
			dup++
		}
		seen[p.Seq] = struct{}{}
	}
	return dup
}

func countReordered(in []rtp.Packet) int {
	n := 0
	for i := 1; i < len(in); i++ {
		if int16(in[i].Seq-in[i-1].Seq) < 0 {
			n++
		}
	}
	return n
}

// packetRecoveryRate is successful deliveries (network or repaired) over
// packets sent. Duplicates are injected on purpose and are not losses.
func packetRecoveryRate(r RTPReport) float64 {
	if r.Sent <= 0 {
		return 0
	}
	rate := float64(r.Delivered) / float64(r.Sent)
	if rate > 1 {
		rate = 1
	}
	return rate
}

// deterministicJitter returns signed nanoseconds jitter for a sequence.
func deterministicJitter(seq uint16, seed, amplitudeMs int64) int64 {
	if amplitudeMs <= 0 {
		return 0
	}
	h := int64(seq)*2654435761 + seed*40503
	span := 2*amplitudeMs*1e6 + 1
	v := h % span
	if v < 0 {
		v += span
	}
	return v - amplitudeMs*1e6
}

// recoveredPrefix counts distinct output frames whose origin sample is still
// available. It is the count of frames the renderer may commit, so an
// overlapping camera handover is never double counted as two frames.
func recoveredPrefix(info *mp4.Info, rec *mp4.Recovery) int {
	if info == nil {
		return 0
	}
	n := 0
	for f := 0; f < fixture.TotalFrames; f++ {
		if localFrameAvailable(info, rec, f) {
			n++
		}
	}
	return n
}

// expectedRenderable counts the output frames whose origin sample survived.
func expectedRenderable(info *mp4.Info, rec *mp4.Recovery) int {
	return recoveredPrefix(info, rec)
}

// sourcePresent reports whether a planned segment's source track exists in
// the parsed container. Marker tracks and audio are tracked by source name,
// so a fixture that omits one cannot silently render a hollow timeline.
func sourcePresent(info *mp4.Info, seg graph.Step) bool {
	switch seg.Track {
	case "video", "voice":
		for _, tr := range info.Tracks {
			if sourceName(tr.ID) == segmentSource(seg.Segment) {
				return true
			}
		}
		return false
	default:
		return true // marker: synthetic overlay, no container track required
	}
}

// segmentSource maps a plan segment id onto a fixture source name.
func segmentSource(segment string) string {
	switch segment {
	case "cam0":
		return fixture.SourceCam0
	case "cam1":
		return fixture.SourceCam1
	case "voice0":
		return fixture.SourceVoice
	}
	return ""
}
