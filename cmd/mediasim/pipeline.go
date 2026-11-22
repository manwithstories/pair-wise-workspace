package main

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"sort"

	"mediasim/internal/clock"
	"mediasim/internal/meta"
	"mediasim/internal/rtp"
	"mediasim/internal/txn"
)

// pipelineData holds the committed-ready segments and the quality
// metrics collected across ingest, repair, clock and alignment.
type pipelineData struct {
	segments      []txn.Segment
	avSyncMs      []float64
	recovered     int
	recoverable   int
	unrecoverable int
	duplicates    int
	late          int
	corrections   int
	marked        int
	alignedEvents int
}

type trackStream struct {
	packets []*rtp.Packet
	media   []int64 // rebased media time per packet
	unrec   [][2]int64
}

// buildPipeline runs generation, ingest, jitter/repair, clock conversion
// and metadata alignment, producing the segment list to commit.
func buildPipeline(cfg *config) (*pipelineData, error) {
	rng := rand.New(rand.NewSource(cfg.seed))
	tl := clock.New()
	for track, rate := range map[string]uint32{"audio": audioRate, "video": videoRate, "meta": 90000} {
		if err := tl.Register(track, rate); err != nil {
			return nil, err
		}
	}
	audio, video := genStreams(rng)
	wire := append(genWire(audio, rng, cfg.loss, cfg.burst), genWire(video, rng, cfg.loss, cfg.burst)...)
	sort.SliceStable(wire, func(i, j int) bool { return wire[i].arrival < wire[j].arrival })

	buffers := map[string]*rtp.Buffer{"audio": rtp.NewBuffer(16), "video": rtp.NewBuffer(16)}
	streams := map[string]*trackStream{"audio": {}, "video": {}}
	data := &pipelineData{}
	repairInto := func(track string, rel *rtp.Release) {
		res := rtp.Repair(rel)
		st := streams[track]
		st.packets = append(st.packets, res.Packets...)
		st.unrec = append(st.unrec, res.Unrecoverable...)
		data.recovered += res.Recovered
		data.recoverable += res.Recoverable
		data.unrecoverable += countRange(res.Unrecoverable)
	}
	for _, wp := range wire {
		p, err := rtp.Parse(wp.data, wp.track, wp.arrival)
		if err != nil {
			return nil, err
		}
		b := buffers[wp.track]
		if !b.Push(p) {
			continue // duplicate or late: counted in the buffer
		}
		repairInto(wp.track, b.Drain())
	}
	for track, b := range buffers {
		repairInto(track, b.Flush())
		data.duplicates += b.Duplicates()
		data.late += b.Late()
	}

	// Convert to the common timeline, rebasing each track at its first
	// frame; recovered packets interpolate between their neighbours.
	drift := clock.NewDriftEstimator(clock.DefaultDriftThresholdPPM, 32)
	for track, st := range streams {
		st.media = make([]int64, len(st.packets))
		var base int64
		baseSet := false
		for i, p := range st.packets {
			if p.Recovered {
				st.media[i] = -1
				continue
			}
			m, err := tl.ToMedia(track, p.Timestamp)
			if err != nil {
				return nil, err
			}
			if !baseSet {
				base, baseSet = m, true
			}
			st.media[i] = m - base
		}
		for i, p := range st.packets {
			if p.Recovered {
				st.media[i] = (st.media[i-1] + st.media[i+1]) / 2
			}
		}
		// Drift observation against the nominal common timeline.
		nominal := nominalMedia(track, audio, video)
		for i, p := range st.packets {
			idx := int(p.ExtSeq - baseExt(track))
			if idx >= 0 && idx < len(nominal) {
				drift.Observe(track, nominal[idx], st.media[i])
			}
		}
		data.corrections += len(drift.Corrections(track))
	}

	// AV sync: nearest audio frame for every video frame.
	am := streams["audio"].media
	for _, mv := range streams["video"].media {
		j := sort.Search(len(am), func(i int) bool { return am[i] >= mv })
		best := int64(1 << 62)
		for _, k := range []int{j - 1, j} {
			if k >= 0 && k < len(am) && abs64(am[k]-mv) < best {
				best = abs64(am[k] - mv)
			}
		}
		data.avSyncMs = append(data.avSyncMs, clock.MediaToMillis(best))
	}

	// Timed metadata: generated events plus parsed emsg boxes.
	events, emsgEvents := genEvents()
	segBytes, err := meta.BuildSegment(emsgEvents)
	if err != nil {
		return nil, err
	}
	parsed, err := meta.ParseSegment(segBytes)
	if err != nil {
		return nil, err
	}
	for _, ev := range parsed {
		events = append(events, meta.Event{
			Kind: meta.Emsg, Track: "meta", RawTS: uint32(ev.PresentationTime),
			Arrival: int64(ev.PresentationTime) - 450, Payload: string(ev.Data),
		})
	}
	aligned, err := meta.NewAligner(tl).Align(events)
	if err != nil {
		return nil, err
	}
	data.alignedEvents = len(aligned)

	// Cut aligned media into commit segments; windows intersecting an
	// unrecoverable range are explicitly marked and never committed.
	unrecMedia := func(track string) [][2]int64 {
		nominal := nominalMedia(track, audio, video)
		base := baseExt(track)
		var out [][2]int64
		for _, r := range streams[track].unrec {
			lo, hi := int(r[0]-base), int(r[1]-base)
			if lo < 0 || hi >= len(nominal) {
				continue
			}
			end := nominal[hi] + 1800
			if hi+1 < len(nominal) {
				end = nominal[hi+1]
			}
			out = append(out, [2]int64{nominal[lo], end})
		}
		return out
	}
	unrec := append(unrecMedia("audio"), unrecMedia("video")...)
	var segID uint64
	for start := int64(0); start < streamMediaTicks; start += segmentTicks {
		end := start + segmentTicks
		damaged := false
		for _, r := range unrec {
			if r[0] < end && start < r[1] {
				damaged = true
				break
			}
		}
		if damaged {
			data.marked++
			segID++
			continue
		}
		payload, nEvents := segmentPayload(streams, aligned, start, end)
		data.segments = append(data.segments, txn.NewSegment(segID, start, end, nEvents, payload))
		segID++
	}
	return data, nil
}

func nominalMedia(track string, audio, video *stream) []int64 {
	s := audio
	if track == "video" {
		s = video
	}
	out := make([]int64, len(s.frames))
	for i, f := range s.frames {
		out[i] = f.media
	}
	return out
}

func baseExt(track string) int64 {
	if track == "audio" {
		return audioBaseSeq
	}
	return videoBaseSeq
}

func countRange(ranges [][2]int64) int {
	n := 0
	for _, r := range ranges {
		n += int(r[1]-r[0]) + 1
	}
	return n
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// segmentPayload serialises the frames and events inside [start, end)
// into a deterministic byte string.
func segmentPayload(streams map[string]*trackStream, events []meta.Event, start, end int64) ([]byte, int) {
	var buf bytes.Buffer
	for _, track := range []string{"audio", "video"} {
		st := streams[track]
		for i, p := range st.packets {
			m := st.media[i]
			if m < start || m >= end {
				continue
			}
			buf.WriteByte(byte(len(track)))
			buf.WriteString(track)
			binary.Write(&buf, binary.BigEndian, uint64(p.ExtSeq))
			binary.Write(&buf, binary.BigEndian, uint64(m))
			binary.Write(&buf, binary.BigEndian, uint16(len(p.Payload)))
			buf.Write(p.Payload)
		}
	}
	nEvents := 0
	for _, ev := range events {
		if ev.MediaTime < start || ev.MediaTime >= end {
			continue
		}
		buf.WriteByte(0xFF)
		buf.WriteByte(byte(ev.Kind))
		binary.Write(&buf, binary.BigEndian, uint64(ev.MediaTime))
		binary.Write(&buf, binary.BigEndian, uint16(len(ev.Payload)))
		buf.WriteString(ev.Payload)
		nEvents++
	}
	return buf.Bytes(), nEvents
}

func p95Ms(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	return s[(len(s)*95+99)/100-1]
}

func lossRecoveryRate(data *pipelineData) float64 {
	if data.recoverable == 0 {
		return 1
	}
	return float64(data.recovered) / float64(data.recoverable)
}
