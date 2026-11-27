package timeline

import (
	"bytes"
	"testing"

	"mediad/internal/media"
)

type memSink struct {
	buf []byte
	off int64
}

func (m *memSink) Write(b []byte) (int, error) {
	if int(m.off)+len(b) > len(m.buf) {
		m.buf = append(m.buf[:m.off], b...)
	} else {
		copy(m.buf[m.off:], b)
	}
	m.off += int64(len(b))
	return len(b), nil
}

func (m *memSink) Truncate(size int64) error {
	m.buf = m.buf[:size]
	m.off = size
	return nil
}

func (m *memSink) Offset() int64 { return m.off }

func mt(n int64) media.MediaTime { return media.MediaTime{Nanos: n, Rate: 48000} }

func ev(track uint32, seq uint64, t int64, data string) *Event {
	return &Event{Time: mt(t), Track: track, ExtSeq: seq, Kind: KindAudio, Data: []byte(data)}
}

func TestWatermarkMonotone(t *testing.T) {
	w := NewWatermark(100)
	if !w.Advance(50) {
		t.Fatal("first advance")
	}
	if !w.Advance(80) {
		t.Fatal("forward advance")
	}
	if w.Advance(80) || w.Advance(30) {
		t.Fatal("watermark moved backward")
	}
	cur, _ := w.Current()
	if cur != 80 {
		t.Fatalf("watermark=%d", cur)
	}
}

func TestTieBreakByTrackThenSeq(t *testing.T) {
	a := ev(2, 1, 100, "a")
	b := ev(1, 9, 100, "b")
	c := ev(1, 3, 100, "c")
	if !Less(b, a) || !Less(c, b) || !Less(c, a) {
		t.Fatal("tie-break must order by track then extended sequence")
	}
}

func TestLateInsideWindowRewinds(t *testing.T) {
	sink := &memSink{}
	tl := NewTimeline(NewWatermark(1000), sink)
	tl.Emit(ev(1, 1, 100, "A"))
	tl.Emit(ev(1, 3, 300, "C"))
	tl.Emit(ev(1, 4, 400, "D"))
	before := append([]byte(nil), sink.buf...)
	_ = before
	// Late event at t=200, inside the 1000ns window.
	if f := tl.Late(ev(1, 2, 200, "B")); f != nil {
		t.Fatal(f)
	}
	if tl.Rewinds != 1 {
		t.Fatalf("rewinds=%d", tl.Rewinds)
	}
	// Re-emitted stream must read A B C D in order.
	sink2 := &memSink{}
	tl2 := NewTimeline(NewWatermark(1000), sink2)
	for _, e := range []*Event{ev(1, 1, 100, "A"), ev(1, 2, 200, "B"), ev(1, 3, 300, "C"), ev(1, 4, 400, "D")} {
		tl2.Emit(e)
	}
	if !bytes.Equal(sink.buf, sink2.buf) {
		t.Fatalf("rewound artifact differs from in-order artifact")
	}
	// Offsets of retained events must be consistent after the rewind.
	off := int64(0)
	for _, e := range tl.Retained() {
		if e.Offset != off {
			t.Fatalf("event seq=%d offset=%d want %d", e.ExtSeq, e.Offset, off)
		}
		off += int64(recordHeader + len(e.Data))
	}
}

func TestLateOutsideWindowRejected(t *testing.T) {
	sink := &memSink{}
	tl := NewTimeline(NewWatermark(100), sink)
	tl.Emit(ev(1, 1, 10000, "A"))
	size := sink.Offset()
	f := tl.Late(ev(1, 2, 100, "B"))
	if f == nil || f.Kind != media.FaultUnrecoverable {
		t.Fatalf("want unrecoverable fault, got %v", f)
	}
	if tl.LateDrops != 1 {
		t.Fatalf("late drops=%d", tl.LateDrops)
	}
	if sink.Offset() != size {
		t.Fatal("artifact was modified by an out-of-window event")
	}
}

func TestEvictionBoundsRetained(t *testing.T) {
	sink := &memSink{}
	tl := NewTimeline(NewWatermark(100), sink)
	for i := int64(0); i < 10; i++ {
		tl.Emit(ev(1, uint64(i), i*100, "x"))
	}
	// Window is 100ns; only events at t>=800-100=700... watermark=900,
	// so events with t < 800 are evicted.
	if n := len(tl.Retained()); n != 2 {
		t.Fatalf("retained=%d want 2", n)
	}
}
