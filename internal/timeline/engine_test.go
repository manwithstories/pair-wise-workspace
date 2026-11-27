package timeline

import (
	"testing"

	"relay/internal/media"
)

func env(track uint32, seq uint64, ns media.TimeNS) *Envelope {
	return &Envelope{TrackID: track, Frame: &media.Frame{Sequence: seq, EventNS: ns, CameraID: uint16(track)}}
}

func TestWatermarkAdvance(t *testing.T) {
	e := NewEngine(media.GraceWindow)
	e.Ingest(env(1, 1, media.Second))
	if got := e.Watermark(1); got != media.Second-media.GraceWindow {
		t.Fatalf("watermark = %d", got)
	}
	e.Ingest(env(1, 2, 2*media.Second))
	if got := e.Watermark(1); got != 2*media.Second-media.GraceWindow {
		t.Fatalf("watermark did not advance: %d", got)
	}
}

func TestLateFrameWithinWindowReordered(t *testing.T) {
	e := NewEngine(media.GraceWindow)
	e.Ingest(env(1, 1, media.Second))
	// 迟到 200ms：仍在 250ms 回溯窗内。
	released := e.Ingest(env(1, 2, media.Second-200*media.Millisecond))
	if len(released) != 0 {
		t.Fatalf("late in-window frame should stay pending, got %d", len(released))
	}
	// 新帧推进水位到 2s-250ms，两帧均跨越水位，且按键有序（迟到帧排前）。
	out := e.Ingest(env(1, 3, 2*media.Second))
	if len(out) != 2 || out[0].Frame.Sequence != 2 || out[1].Frame.Sequence != 1 {
		t.Fatalf("deterministic reorder wrong: %v", out)
	}
}

func TestLateFrameBeyondWindowQuarantined(t *testing.T) {
	e := NewEngine(media.GraceWindow)
	e.Ingest(env(1, 1, media.Second))
	e.Ingest(env(1, 2, 2*media.Second)) // 水位 = 1.75s
	// 迟到 400ms：1.6s < 1.75s，超出回溯窗。
	e.Ingest(env(1, 3, 2*media.Second-400*media.Millisecond))
	q := e.Quarantine()
	if len(q) != 1 || q[0].Reason != ReasonLateBeyondWindow {
		t.Fatalf("quarantine = %+v", q)
	}
	if q[0].Env.Frame.Sequence != 3 {
		t.Fatalf("wrong frame quarantined")
	}
}

func TestIndependentTrackWatermarks(t *testing.T) {
	e := NewEngine(media.GraceWindow)
	e.Ingest(env(1, 1, media.Second))
	e.Ingest(env(2, 1, 5*media.Second))
	if e.Watermark(1) != media.Second-media.GraceWindow {
		t.Fatal("track 1 watermark disturbed by track 2")
	}
	if e.Watermark(2) != 5*media.Second-media.GraceWindow {
		t.Fatal("track 2 watermark wrong")
	}
}

func TestFlushIsGloballySorted(t *testing.T) {
	e := NewEngine(media.GraceWindow)
	// 三条事件时间都落在 250ms 回溯窗内，均不会在 Ingest 时提前排出。
	e.Ingest(env(2, 1, media.Second+100*media.Millisecond))
	e.Ingest(env(1, 1, media.Second))
	e.Ingest(env(1, 2, media.Second+200*media.Millisecond))
	out := e.Flush()
	if len(out) != 3 {
		t.Fatalf("flush count = %d", len(out))
	}
	for i := 1; i < len(out); i++ {
		if LessKey(out[i].Key(), out[i-1].Key()) {
			t.Fatalf("flush not sorted at %d", i)
		}
	}
}

func TestStateExportRestore(t *testing.T) {
	e := NewEngine(media.GraceWindow)
	e.Ingest(env(1, 1, media.Second))
	st := e.ExportState()
	e2 := NewEngine(0)
	e2.RestoreState(st)
	if e2.Watermark(1) != media.Second-media.GraceWindow {
		t.Fatalf("restored watermark = %d", e2.Watermark(1))
	}
	if e2.PendingCount() != 0 {
		t.Fatal("restore assumes empty holdback at checkpoint")
	}
}
