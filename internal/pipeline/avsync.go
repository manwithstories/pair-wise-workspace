package pipeline

import (
	"github.com/pion/rtp"

	"mediaqa/internal/media"
	rtpwin "mediaqa/internal/rtp"
)

// RTPPacket is a delivered packet with its unwrapped clock values.
type RTPPacket struct {
	Packet    *rtp.Packet
	ExtSeq    uint64
	Timestamp media.Rational // unwrapped RTP timestamp, seconds
}

// RTPResult is the outcome of recovering one RTP stream.
type RTPResult struct {
	Delivered []RTPPacket
	Stats     rtpwin.Stats
}

// RecoverRTP runs the bounded reorder/recovery window over received
// packets (cache holds retransmission copies) and writes losses back
// into the shared error model, with timestamps mapped onto the rational
// time base at clockRate.
func RecoverRTP(trackID string, clockRate uint32, received, cache []*rtp.Packet, errs *media.List) RTPResult {
	w := rtpwin.New(64, 256)
	for _, p := range cache {
		w.CacheAdd(p)
	}
	var res RTPResult
	var prevTS uint64
	started := false
	emit := func(pkts []*rtp.Packet) {
		for _, p := range pkts {
			if !started {
				prevTS = uint64(p.Timestamp)
				started = true
			}
			prevTS = media.Unwrap32(p.Timestamp, prevTS)
			res.Delivered = append(res.Delivered, RTPPacket{
				Packet:    p,
				ExtSeq:    media.Unwrap16(p.SequenceNumber, uint64(p.SequenceNumber)),
				Timestamp: media.FromTicks(int64(prevTS), int64(clockRate)),
			})
		}
	}
	for _, p := range received {
		emit(w.Push(p))
	}
	emit(w.Finish())
	res.Stats = w.Stats
	if w.Stats.Lost > 0 {
		errs.Addf(media.SeverityRecoverable, trackID, "rtp-loss",
			media.Rational{}, media.Rational{},
			"%d packet(s) unrecoverable, %d recovered from retransmit cache",
			w.Stats.Lost, w.Stats.Recovered)
	}
	return res
}

// AVSyncMaxMS returns the maximum absolute presentation-time difference
// between each video sample and its nearest audio sample, in
// milliseconds, on the unified rational clock.
func AVSyncMaxMS(video, audio TrackResult) float64 {
	if len(video.Samples) == 0 || len(audio.Samples) == 0 {
		return 0
	}
	// Presentation times of audio samples, in rational seconds.
	type pt struct{ t media.Rational }
	audioPres := make([]media.Rational, 0, len(audio.Samples))
	for _, s := range audio.Samples {
		if p, ok := audio.Mapper.MediaToPres(s.PTS); ok {
			audioPres = append(audioPres, media.FromTicks(p, audio.Timescale))
		}
	}
	if len(audioPres) == 0 {
		return 0
	}
	maxMS := 0.0
	for _, s := range video.Samples {
		p, ok := video.Mapper.MediaToPres(s.PTS)
		if !ok {
			continue
		}
		vp := media.FromTicks(p, video.Timescale)
		// Linear scan for nearest (sample counts are bounded here).
		best := media.Rational{}
		first := true
		for _, ap := range audioPres {
			d := ap.Sub(vp)
			if d.Num < 0 {
				d = media.NewRational(-d.Num, d.Den)
			}
			if first || d.Cmp(best) < 0 {
				best, first = d, false
			}
		}
		if ms := best.Millis(); ms > maxMS {
			maxMS = ms
		}
	}
	return maxMS
}
