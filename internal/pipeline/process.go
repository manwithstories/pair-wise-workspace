// Package pipeline: input processing path of the engine.
package pipeline

import (
	"encoding/binary"
	"fmt"

	"mediad/internal/ingest"
	"mediad/internal/jitter"
	"mediad/internal/media"
	"mediad/internal/normalize"
	"mediad/internal/recovery"
	"mediad/internal/timeline"
)

func (e *Engine) ProcessRTP(raw []byte) {
	at := e.arrival
	e.arrival++
	pkt, f := e.reg.Ingest(raw, at)
	if f != nil {
		e.noteFault(f)
		return
	}
	class, f := e.recov[pkt.Track].Observe(pkt.ExtSeq, at)
	if f != nil {
		e.noteFault(f)
	}
	switch class {
	case recovery.ClassDuplicate:
		e.counters.Duplicates++
		return
	case recovery.ClassGapFill:
		e.counters.Reordered++
	default:
		if f != nil && f.Kind == media.FaultUnrecoverable {
			e.counters.Loss += f.Count
			e.counters.Unrecoverable += f.Count
		}
	}
	cm := e.clocks[pkt.Track]
	if !cm.Anchored() {
		// No Sender Report yet: the packet cannot be placed on the
		// unified timeline, so it is held until the clock is anchored.
		e.pending[pkt.Track] = append(e.pending[pkt.Track],
			pendingItem{pkt: pkt, fault: f, gapFill: class == recovery.ClassGapFill})
		e.maybeCheckpoint()
		return
	}
	if class == recovery.ClassGapFill {
		// A recovered-late packet rejoins the reorder buffer when it
		// still fits; only when it is too old for the buffer does it go
		// straight to the timeline, which rewinds or rejects it.
		mt := cm.ToMediaTime(pkt.ExtTS)
		q := e.queues[pkt.Track]
		if ok, _ := q.Fits(mt, len(pkt.Payload)); ok {
			if df := q.Push(pkt, mt); df != nil {
				e.noteFault(df)
			}
			e.drain(false)
		} else {
			e.emitPacket(pkt, mt)
		}
		e.maybeCheckpoint()
		return
	}
	mt := cm.ToMediaTime(pkt.ExtTS)
	e.notePush(pkt.Track, mt.Nanos)
	if f != nil && f.Kind == media.FaultUnrecoverable {
		e.emitEvent(&timeline.Event{
			Time: mt, Track: pkt.Track, ExtSeq: f.Seq,
			Kind: timeline.KindGap, Data: []byte(f.Msg), At: at,
		})
	}
	if df := e.queues[pkt.Track].Push(pkt, mt); df != nil {
		e.noteFault(df)
	}
	e.drain(false)
	e.maybeCheckpoint()
}

func (e *Engine) maybeCheckpoint() {
	if e.cfg.CheckpointEvery > 0 && e.arrival%int64(e.cfg.CheckpointEvery) == 0 {
		if err := e.Checkpoint(); err != nil && e.ckptErr == nil {
			e.ckptErr = err
		}
	}
}

// CheckpointError returns the first checkpoint failure, if any.
func (e *Engine) CheckpointError() error { return e.ckptErr }

// flushPending releases packets held for a track once its clock is
// anchored by a Sender Report.
func (e *Engine) flushPending(track uint32) {
	items := e.pending[track]
	if len(items) == 0 {
		return
	}
	cm := e.clocks[track]
	if !cm.Anchored() {
		return
	}
	delete(e.pending, track)
	for _, it := range items {
		mt := cm.ToMediaTime(it.pkt.ExtTS)
		e.notePush(track, mt.Nanos)
		if it.fault != nil && it.fault.Kind == media.FaultUnrecoverable {
			e.emitEvent(&timeline.Event{
				Time: mt, Track: track, ExtSeq: it.fault.Seq,
				Kind: timeline.KindGap, Data: []byte(it.fault.Msg), At: it.pkt.At,
			})
		}
		if df := e.queues[track].Push(it.pkt, mt); df != nil {
			e.noteFault(df)
		}
	}
	e.drain(false)
}

// ProcessRTCP ingests one raw RTCP datagram (clock reports).
func (e *Engine) ProcessRTCP(raw []byte) {
	at := e.arrival
	e.arrival++
	faults, err := ingest.ParseRTCP(raw, at, e.clocks)
	if err != nil {
		e.noteFault(err.(*media.Fault))
		return
	}
	for _, f := range faults {
		e.noteFault(f)
	}
	for _, ssrc := range e.trackOrder {
		e.flushPending(ssrc)
	}
	e.maybeCheckpoint()
}

// notePush records the newest pushed event time of a track and moves
// the emission horizon to the minimum across tracks, so one track's
// clock jump cannot inflate the horizon and cause premature emission.
func (e *Engine) notePush(track uint32, nanos int64) {
	if nanos > e.lastPush[track] {
		e.lastPush[track] = nanos
	}
	first := true
	for _, ssrc := range e.trackOrder {
		v, ok := e.lastPush[ssrc]
		if !ok {
			continue
		}
		if first || v < e.maxPushed {
			e.maxPushed = v
		}
		first = false
	}
}

// drain emits queued events whose time is covered by the emission
// horizon minus the drain slack. With force it empties every queue.
func (e *Engine) drain(force bool) {
	for {
		var bq *jitter.Queue
		var bt media.MediaTime
		found := false
		for _, ssrc := range e.trackOrder {
			q := e.queues[ssrc]
			_, t, ok := q.Head()
			if !ok {
				continue
			}
			if !found || media.Compare(t, bt) < 0 {
				bq, bt, found = q, t, true
			}
		}
		if !found {
			return
		}
		if !force && bt.Nanos > e.maxPushed-e.cfg.DrainSlackNanos {
			return
		}
		pkt, t, _ := bq.Pop()
		e.emitPacket(pkt, t)
	}
}

func bytesToSamples(b []byte) []int16 {
	n := len(b) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}

func samplesToBytes(s []int16) []byte {
	out := make([]byte, 2*len(s))
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

// emitPacket normalizes one dequeued packet and emits its event.
func (e *Engine) emitPacket(pkt *ingest.Packet, t media.MediaTime) {
	p, _ := e.reg.Params(pkt.Track)
	ev := &timeline.Event{Time: t, Track: pkt.Track, ExtSeq: pkt.ExtSeq, At: pkt.At}
	if p.IsVideo {
		c := e.colors.Confirmed(pkt.Track)
		hdr := fmt.Sprintf("color=%s,%s,%s,%s;", c.Primaries, c.Transfer, c.Matrix, c.Range)
		ev.Kind = timeline.KindVideo
		ev.Data = append([]byte(hdr), pkt.Payload...)
	} else {
		st, f := normalize.Downmix(bytesToSamples(pkt.Payload), p.Channels, normalize.Layout(p.Layout))
		if f != nil {
			f.Track, f.Seq, f.At = pkt.Track, pkt.ExtSeq, pkt.At
			e.noteFault(f)
			e.counters.LayoutErrors++
			ev.Kind = timeline.KindDiagnostic
			ev.Data = []byte(f.Msg)
		} else {
			ev.Kind = timeline.KindAudio
			ev.Data = samplesToBytes(e.resamp[pkt.Track].Process(st))
		}
	}
	e.emitEvent(ev)
}

// emitEvent routes an event to the ordered emitter or the replay path.
func (e *Engine) emitEvent(ev *timeline.Event) {
	if cur, ok := e.tl.Watermark().Current(); ok && ev.Time.Nanos < cur {
		if f := e.tl.Late(ev); f != nil {
			f.At = ev.At
			e.noteFault(f)
			e.counters.Unrecoverable++
		}
		return
	}
	e.tl.Emit(ev)
}

// Flush drains all queues and declares every still-open gap
// unrecoverable. Called once at end of stream.
func (e *Engine) Flush() {
	e.drain(true)
	for _, ssrc := range e.trackOrder {
		left := e.recov[ssrc].Finalize()
		if len(left) == 0 {
			continue
		}
		e.counters.Loss += int64(len(left))
		e.counters.Unrecoverable += int64(len(left))
		cur, ok := e.tl.Watermark().Current()
		if !ok {
			continue
		}
		for _, s := range left {
			e.emitEvent(&timeline.Event{
				Time:  media.MediaTime{Nanos: cur, Rate: 1},
				Track: ssrc, ExtSeq: s, Kind: timeline.KindGap,
				Data: []byte("unrecoverable gap at end of stream"),
				At:   e.arrival,
			})
		}
	}
}

// Summary builds the run report; the caller fills in the fields that
// need ground truth or wall-clock measurements.
