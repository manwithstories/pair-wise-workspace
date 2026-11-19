package pipeline

import (
	"sort"

	"mediamix/internal/core"
	"mediamix/internal/fixture"
	"mediamix/internal/timeline"
	"mediamix/internal/transport"
)

// medianMediaOffset returns the median (arrival - intended presentation) over
// delivered packets. Positive means media is late relative to its own clock.
func medianMediaOffset(deliveries []fixture.Delivery, delivered []transport.Packet) (timeline.Micros, bool) {
	if len(delivered) == 0 {
		return 0, false
	}
	bySeq := make(map[uint16]fixture.Delivery, len(deliveries))
	for _, d := range deliveries {
		bySeq[d.Packet.Seq] = d
	}
	offs := make([]timeline.Micros, 0, len(delivered))
	for _, p := range delivered {
		d, ok := bySeq[p.Seq]
		if !ok {
			continue
		}
		offs = append(offs, d.ArrivalUs-d.Packet.SentUs)
	}
	if len(offs) == 0 {
		return 0, false
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	return offs[len(offs)/2], true
}

// p95Latency estimates the 95th percentile receiver latency: arrival minus the
// sender's intended presentation time, clamped at zero. Retransmitted packets
// contribute one recovery deadline, which is what they actually cost.
func p95Latency(deliveries []fixture.Delivery, delivered []transport.Packet) float64 {
	if len(delivered) == 0 {
		return 0
	}
	bySeq := make(map[uint16]fixture.Delivery, len(deliveries))
	for _, d := range deliveries {
		bySeq[d.Packet.Seq] = d
	}
	lat := make([]float64, 0, len(delivered))
	for _, p := range delivered {
		if d, ok := bySeq[p.Seq]; ok {
			delta := float64(d.ArrivalUs-d.Packet.SentUs) / 1000
			if delta < 0 {
				delta = 0
			}
			lat = append(lat, delta)
			continue
		}
		if p.Retransmit {
			lat = append(lat, transport.DefaultRecoveryPolicy().DeadlineMs)
		}
	}
	if len(lat) == 0 {
		return 0
	}
	sort.Float64s(lat)
	return percentile(lat, 0.95)
}

func toPacket(track string, p fixture.SentPacket, arrival timeline.Micros) transport.Packet {
	return transport.Packet{
		Track: track, Seq: p.Seq, Timestamp: p.Timestamp,
		Marker: p.Marker, Keyframe: p.Keyframe, Payload: p.Payload, ArrivalUs: arrival,
	}
}

func lastArrival(deliveries []fixture.Delivery) timeline.Micros {
	var last timeline.Micros
	for _, d := range deliveries {
		if d.ArrivalUs > last {
			last = d.ArrivalUs
		}
	}
	return last
}

func findSpec(fx *fixture.Fixture, id string) *fixture.TrackSpec {
	for i := range fx.Tracks {
		if fx.Tracks[i].ID == id {
			return &fx.Tracks[i]
		}
	}
	return nil
}

// fail marks a result failed with the given error.
func (r *TrackResult) fail(err error) {
	e, ok := core.As(err)
	if !ok {
		e = &core.Error{Code: core.CodeInternal, Track: r.Track, Detail: err.Error(), Fatal: true}
	}
	if e.Track == "" {
		e.Track = r.Track
	}
	r.Status = StatusFailed
	r.Error = e
}
