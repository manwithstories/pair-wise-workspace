package timeline

import (
	"encoding/binary"
	"sort"

	"mediad/internal/media"
)

// Kind classifies a timeline event record in the output artifact.
type Kind uint8

const (
	KindAudio Kind = iota + 1
	KindVideo
	KindGap
	KindDiagnostic
)

// Event is one finalized record on the unified timeline. Data holds the
// already-normalized payload; it is frozen at normalization time so
// replays re-emit identical bytes.
type Event struct {
	Time   media.MediaTime
	Track  uint32
	ExtSeq uint64
	Kind   Kind
	Data   []byte
	At     int64 // input arrival index, for locating
	Offset int64 // artifact offset assigned at emission
}

// Less orders events by media time, then track, then extended sequence
// number, so ties always resolve the same way.
func Less(a, b *Event) bool {
	if c := media.Compare(a.Time, b.Time); c != 0 {
		return c < 0
	}
	if a.Track != b.Track {
		return a.Track < b.Track
	}
	return a.ExtSeq < b.ExtSeq
}

const recordHeader = 8 + 4 + 8 + 1 + 4 // time, track, seq, kind, datalen

// Encode serializes an event into its artifact record.
func Encode(ev *Event) []byte {
	buf := make([]byte, recordHeader+len(ev.Data))
	binary.BigEndian.PutUint64(buf[0:8], uint64(ev.Time.Nanos))
	binary.BigEndian.PutUint32(buf[8:12], ev.Track)
	binary.BigEndian.PutUint64(buf[12:20], ev.ExtSeq)
	buf[20] = byte(ev.Kind)
	binary.BigEndian.PutUint32(buf[21:25], uint32(len(ev.Data)))
	copy(buf[25:], ev.Data)
	return buf
}

// Sink is the append-only artifact writer backing the timeline.
type Sink interface {
	Write(b []byte) (int, error)
	Truncate(size int64) error
	Offset() int64
}

// Timeline emits events in order and re-emits deterministically when
// late data lands inside the rewind window.
type Timeline struct {
	wm        *Watermark
	sink      Sink
	retained  []*Event // emitted events still inside the window
	Rewinds   int64
	LateDrops int64 // events rejected as unrecoverable
}

func NewTimeline(wm *Watermark, sink Sink) *Timeline {
	return &Timeline{wm: wm, sink: sink}
}

// Restore rebuilds a timeline from checkpoint state.
func Restore(wm *Watermark, sink Sink, retained []*Event, rewinds, lateDrops int64) *Timeline {
	return &Timeline{wm: wm, sink: sink, retained: retained, Rewinds: rewinds, LateDrops: lateDrops}
}

func (t *Timeline) Watermark() *Watermark { return t.wm }

// Retained returns the events still inside the rewind window.
func (t *Timeline) Retained() []*Event { return append([]*Event(nil), t.retained...) }

func (t *Timeline) write(ev *Event) {
	ev.Offset = t.sink.Offset()
	t.sink.Write(Encode(ev))
}

// Emit appends an event whose time is at or beyond the watermark.
func (t *Timeline) Emit(ev *Event) {
	t.wm.Advance(ev.Time.Nanos)
	t.write(ev)
	t.retained = append(t.retained, ev)
	t.evict()
}

func (t *Timeline) evict() {
	cur, ok := t.wm.Current()
	if !ok {
		return
	}
	cut := 0
	for cut < len(t.retained) && t.retained[cut].Time.Nanos < cur-t.wm.window {
		cut++
	}
	if cut > 0 {
		t.retained = append([]*Event(nil), t.retained[cut:]...)
	}
}

// Late handles an event whose time is behind the watermark. Inside the
// rewind window the affected suffix is truncated and deterministically
// re-emitted; outside it the event is rejected as unrecoverable and the
// artifact is left untouched.
func (t *Timeline) Late(ev *Event) *media.Fault {
	if !t.wm.InWindow(ev.Time.Nanos) {
		t.LateDrops++
		return &media.Fault{
			Kind: media.FaultUnrecoverable, Op: "timeline.replay",
			Track: ev.Track, Seq: ev.ExtSeq,
			Msg: "event arrived outside the rewind window",
		}
	}
	pos := sort.Search(len(t.retained), func(i int) bool {
		return !Less(t.retained[i], ev)
	})
	t.retained = append(t.retained, nil)
	copy(t.retained[pos+1:], t.retained[pos:])
	t.retained[pos] = ev
	t.Rewinds++
	// The event previously at pos (now at pos+1) marks the first byte
	// that must be rewritten; appending at the tail rewrites nothing.
	var trunc int64
	if pos+1 < len(t.retained) {
		trunc = t.retained[pos+1].Offset
	} else {
		trunc = t.sink.Offset()
	}
	t.sink.Truncate(trunc)
	for _, e := range t.retained[pos:] {
		t.write(e)
	}
	return nil
}
