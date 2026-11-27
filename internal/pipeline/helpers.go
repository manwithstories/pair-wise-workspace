// Package pipeline helpers support the bounded orchestrator: per-fragment
// sizing, track lookup and the round-trip verification used by Run.
package pipeline

import (
	"pairwise/ingestcheck/internal/clock"
	"pairwise/ingestcheck/internal/mp4"
	"pairwise/ingestcheck/internal/recovery"
	rtpadapt "pairwise/ingestcheck/internal/rtp"
	"pairwise/ingestcheck/internal/track"
)

func (r *Runner) isolate(out *trackOut, rep *Report, in FragmentInput, err error) {
	out.report.Isolated++
	rep.Isolated++
	iso := classify(in, err)
	rep.IsolationCodes = append(rep.IsolationCodes, iso.Code.String())
	if iso.Offset >= 0 && (rep.FirstBadByte < 0 || iso.Offset < rep.FirstBadByte) {
		rep.FirstBadByte = iso.Offset
	}
}

func countRecovery(tr *TrackReport, rep *Report, out recovery.Outcome) {
	if out == recovery.Recovered {
		tr.Recovered++
		rep.Recovered++
	} else {
		tr.Explicit++
		rep.ExplicitGap++
	}
}

// fragmentBytes estimates the on-wire size of a fragment from its sample
// sizes; it sizes the re-fetch budget without inventing payload.
func fragmentBytes(f *mp4.Fragment) int64 {
	var total int64
	for _, run := range f.Truns {
		for _, s := range run.Samples {
			total += int64(s.Size)
		}
	}
	if total <= 0 {
		total = 1
	}
	return total
}

// fragmentMicros sums a fragment's sample durations expressed as wall time.
func fragmentMicros(f *mp4.Fragment, rate clock.Rate) int64 {
	var ticks int64
	for _, run := range f.Truns {
		for _, s := range run.Samples {
			ticks += int64(s.Duration)
		}
	}
	if ticks <= 0 {
		return 0
	}
	us, err := rate.Micros(ticks)
	if err != nil {
		return 0
	}
	return us
}

func trackRate(g *track.Graph, id uint32) clock.Rate {
	if n, ok := g.Node(id); ok && n.Rate > 0 {
		if r, err := clock.NewRate(n.Rate, 1); err == nil {
			return r
		}
	}
	return clock.CDN()
}

func videoTrack(g *track.Graph) uint32 {
	for _, id := range g.Order() {
		if g.KindOf(id) == track.Video {
			return id
		}
	}
	return 0
}

func (r *Runner) feedPackets(w *rtpadapt.Window, in FragmentInput) int {
	emitted := 0
	for _, d := range in.Packets {
		pkts, err := w.Feed(d, int64(in.Seq), in.TrackID)
		if err != nil {
			continue
		}
		emitted += len(pkts)
	}
	return emitted
}

func roundTrip(boxes []*mp4.Box, cfg mp4.ParseConfig) (bool, error) {
	var buf []byte
	for _, b := range boxes {
		buf = append(buf, b.Bytes()...)
	}
	again, err := mp4.Parse(buf, cfg)
	if err != nil {
		return false, err
	}
	var check []byte
	for _, b := range again {
		check = append(check, b.Bytes()...)
	}
	return string(check) == string(buf), nil
}

func parseOne(data []byte, cfg mp4.ParseConfig) ([]*mp4.Box, error) {
	if len(data) == 0 {
		return nil, clock.At(clock.CodeBoxTruncated, "pipeline", 0, "empty fragment")
	}
	return mp4.Parse(data, cfg)
}

func classify(in FragmentInput, err error) Isolation {
	iso := Isolation{TrackID: in.TrackID, Seq: in.Seq, Offset: 0, Code: clock.CodeOf(err), Detail: err.Error()}
	if e, ok := err.(*clock.Error); ok && e.Offset >= 0 {
		iso.Offset = e.Offset
	}
	return iso
}
