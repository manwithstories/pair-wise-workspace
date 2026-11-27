package pipeline

import (
	"io"
	"os"

	"mediad/internal/checkpoint"
	"mediad/internal/ingest"
	"mediad/internal/media"
	"mediad/internal/timeline"
)

// FileSink writes the artifact to a file with offset tracking and
// truncation support for replays and checkpoint restores.
type FileSink struct {
	f   *os.File
	off int64
}

func NewFileSink(f *os.File) *FileSink { return &FileSink{f: f} }

func (s *FileSink) Write(b []byte) (int, error) {
	n, err := s.f.Write(b)
	s.off += int64(n)
	return n, err
}

func (s *FileSink) Truncate(size int64) error {
	if err := s.f.Truncate(size); err != nil {
		return err
	}
	if _, err := s.f.Seek(size, io.SeekStart); err != nil {
		return err
	}
	s.off = size
	return nil
}

func (s *FileSink) Offset() int64 { return s.off }

// Checkpoint atomically saves the full recovery state: watermark,
// packet sequence state, clock mappings, resampler positions, queued
// and retained replay events and the artifact offset. Queues are
// snapshotted as-is; forcing emission here would push the watermark
// past still-arriving tracks and corrupt the replay discipline.
func (e *Engine) Checkpoint() error {
	st := &checkpoint.State{
		Generation:   e.gen + 1,
		Arrival:      e.arrival,
		MaxPushed:    e.maxPushed,
		OutputOffset: e.sink.Offset(),
		Retained:     e.tl.Retained(),
		Counters:     e.counters,
	}
	st.Watermark, st.WatermarkSet = e.tl.Watermark().Current()
	st.Counters.Rewinds = e.tl.Rewinds
	st.Counters.LateDrops = e.tl.LateDrops
	st.Colors, st.Diagnostics = e.colors.Snapshot()
	for _, ssrc := range e.trackOrder {
		for _, entry := range e.queues[ssrc].Snapshot() {
			st.Queued = append(st.Queued, checkpoint.QueuedPacket{
				Track: ssrc, Packet: entry.Packet, Time: entry.Time,
			})
		}
	}
	for _, ssrc := range e.trackOrder {
		p, _ := e.reg.Params(ssrc)
		snap := e.reg.Track(ssrc).Snapshot()
		cs := e.clocks[ssrc].Snapshot()
		tr := e.recov[ssrc]
		st.Tracks = append(st.Tracks, checkpoint.TrackState{
			SSRC:         ssrc,
			ClockRate:    p.ClockRate,
			Channels:     p.Channels,
			Layout:       p.Layout,
			IsVideo:      p.IsVideo,
			Started:      snap.Started,
			MaxSeq:       snap.MaxSeq,
			SeqROC:       snap.SeqROC,
			MaxTS:        snap.MaxTS,
			TSROC:        snap.TSROC,
			Anchored:     cs.Anchored,
			RTPAnchor:    cs.RTPAnchor,
			NTPAnchor:    cs.NTPAnchor,
			Jumps:        cs.Jumps,
			Expected:     tr.Expected(),
			Missing:      tr.Missing(),
			Stats:        tr.Stats(),
			ResamplerPos: e.resamp[ssrc].State(),
			LastPush:     e.lastPush[ssrc],
		})
	}
	if err := e.store.Save(st); err != nil {
		return err
	}
	e.gen = st.Generation
	return nil
}

// Restore loads the most recent valid checkpoint, truncates the
// artifact to the recorded offset and rebuilds all processing state.
// A corrupt newest generation falls back to the previous one and the
// fallback fault is returned alongside.
func (e *Engine) Restore() (*media.Fault, error) {
	st, fb, err := e.store.Load()
	if err != nil {
		return fb, err
	}
	if err := e.sink.Truncate(st.OutputOffset); err != nil {
		return fb, err
	}
	e.arrival = st.Arrival
	e.maxPushed = st.MaxPushed
	e.counters = st.Counters
	e.gen = st.Generation
	for _, ts := range st.Tracks {
		e.RegisterTrack(ingest.TrackParams{
			SSRC: ts.SSRC, ClockRate: ts.ClockRate,
			Channels: ts.Channels, Layout: ts.Layout, IsVideo: ts.IsVideo,
		})
		e.reg.Track(ts.SSRC).Restore(ingest.TrackSnapshot{
			Started: ts.Started, MaxSeq: ts.MaxSeq, SeqROC: ts.SeqROC,
			MaxTS: ts.MaxTS, TSROC: ts.TSROC,
		})
		e.clocks[ts.SSRC].Restore(ingest.ClockSnapshot{
			Anchored: ts.Anchored, RTPAnchor: ts.RTPAnchor,
			NTPAnchor: ts.NTPAnchor, Jumps: ts.Jumps,
		})
		e.recov[ts.SSRC].Restore(ts.Expected, ts.Missing)
		e.recov[ts.SSRC].SetStats(ts.Stats)
		e.resamp[ts.SSRC].SetState(ts.ResamplerPos)
		e.lastPush[ts.SSRC] = ts.LastPush
	}
	for _, qp := range st.Queued {
		if q, ok := e.queues[qp.Track]; ok {
			q.Push(qp.Packet, qp.Time)
		}
	}
	e.colors.Restore(st.Colors, st.Diagnostics)
	wm := timeline.NewWatermark(e.cfg.RewindWindowNanos)
	wm.Restore(st.Watermark, st.WatermarkSet)
	e.tl = timeline.Restore(wm, e.sink, st.Retained, st.Counters.Rewinds, st.Counters.LateDrops)
	return fb, nil
}
