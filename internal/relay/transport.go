package relay

import (
	"fmt"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/fixture"
	"relay.local/relay/internal/index"
	"relay.local/relay/internal/model"
	"relay.local/relay/internal/scene"
	"relay.local/relay/internal/transport"
)

type streamResult struct {
	name         string
	expected     int // distinct packets the generator actually sent
	expectedLost int
	got          transport.Result
}

// ok reports whether recovery matched the ground truth exactly.
func (r streamResult) ok() bool {
	return r.got.Stats.Unique == r.expected &&
		r.got.Stats.Dropped == 0 &&
		r.got.Stats.Lost == r.lostExpected()
}

func (r streamResult) lostExpected() int {
	return r.expectedLost
}

// recoverStreams recovers every generated stream through the real wire
// encoding and aggregates wrap counts and client-side statistics.
func recoverStreams(sc *scene.Scenario, rep *Report) []streamResult {
	out := make([]streamResult, 0, len(sc.RTP))
	recovered, total := 0, 0
	for _, s := range sc.RTP {
		res := recoverOne(sc, s)
		expected := s.Stream.Total - s.Stream.Dropped
		sr := streamResult{name: s.Name, expected: expected, got: res}
		sr.expectedLost = s.Stream.Dropped
		out = append(out, sr)
		rep.Wraps += res.Stats.Wraps
		if res.Stats.Recovered > 0 {
			total++
			if sr.ok() {
				recovered++
			}
		}
		for _, it := range res.Faults.Items {
			rep.Faults.Add(it)
		}
	}
	if total == 0 {
		total = len(sc.RTP)
	}
	if recovered == 0 && len(out) > 0 {
		// Count streams that recovered cleanly even if no packet happened to
		// be out of order.
		recovered = 0
		for _, sr := range out {
			if sr.ok() {
				recovered++
			}
		}
	}
	rep.Recovered = fmt.Sprintf("%d/%d", recovered, total)
	return out
}

// recoverOne round-trips one stream through pion/rtp and transport.Recover.
func recoverOne(_ *scene.Scenario, s scene.RTPStream) transport.Result {
	bufs, err := fixture.Marshal(s.Stream.Packets)
	if err != nil {
		res := transport.Result{Faults: &fault.List{}}
		res.Faults.Add(fault.Wrap(-1, 0, fault.KindCorrupt, false, err))
		return res
	}
	wire, err := fixture.Unmarshal(bufs)
	if err != nil {
		res := transport.Result{Faults: &fault.List{}}
		res.Faults.Add(fault.Wrap(-1, 0, fault.KindCorrupt, false, err))
		return res
	}
	parsed := make([]transport.Packet, 0, len(wire))
	for i, p := range wire {
		parsed = append(parsed, transport.Packet{
			Seq: p.SequenceNumber, Timestamp: p.Timestamp, Marker: p.Marker,
			TrackID: scene.RTPTrackID, Payload: p.Payload, Arrival: i + 1,
		})
	}
	return transport.Recover(parsed, transport.Options{
		Window: 8, MaxPackets: 1 << 16, MaxPayload: 1 << 20,
	})
}

// buildIndex builds and validates the keyframe index over video tracks.
func buildIndex(sc *scene.Scenario, rep *Report) {
	total, hits := 0, 0
	for _, clip := range []scene.Clip{sc.MP4, sc.MXF} {
		if clip.File == nil {
			continue
		}
		video := videoTracks(clip.File.Tracks)
		idx := index.Build(video, clip.File.Data)
		total += idx.Count()
		for _, e := range idx.Entries {
			tr, ok := trackByID(clip.File.Tracks, e.TrackID)
			if !ok {
				continue
			}
			if s, ok := tr.SampleByIndex(e.SampleIdx); ok && s.Sync && s.Offset == e.Offset {
				hits++
			}
		}
		rep.VFRIntervals = maxInt(rep.VFRIntervals, distinctDurations(video))
	}
	rep.Keyframe = fmt.Sprintf("%d/%d", hits, total)
}

func videoTracks(tracks []*model.Track) []*model.Track {
	out := make([]*model.Track, 0, len(tracks))
	for _, t := range tracks {
		if t.Kind == model.Video {
			out = append(out, t)
		}
	}
	return out
}

func distinctDurations(tracks []*model.Track) int {
	set := map[int64]bool{}
	for _, t := range tracks {
		for _, s := range t.Samples {
			set[s.Duration] = true
		}
	}
	return len(set)
}

func trackByID(tracks []*model.Track, id uint32) (*model.Track, bool) {
	for _, t := range tracks {
		if t.ID == id {
			return t, true
		}
	}
	return nil, false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
