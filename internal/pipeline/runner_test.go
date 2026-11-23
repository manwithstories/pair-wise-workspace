package pipeline

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"medpipe/internal/model"
	"medpipe/internal/rtp"
)

func buildFrames(n int) []*model.Frame {
	out := make([]*model.Frame, n)
	for i := range out {
		payload := make([]byte, 32)
		binary.BigEndian.PutUint32(payload, uint32(i))
		// Repeat every 12 payloads so dedup has something to reuse.
		binary.BigEndian.PutUint32(payload, uint32(i%12))
		out[i] = &model.Frame{
			Seq: uint32(i), TimestampNS: int64(i) * 40_000_000,
			Keyframe: i%30 == 0, Payload: payload,
		}
	}
	return out
}

func buildAudio(n int) []*model.AudioBlock {
	// Two audio blocks per 40 ms video frame, keeping both tracks on one clock.
	out := make([]*model.AudioBlock, n*2)
	for i := range out {
		payload := make([]byte, 320) // 160 silent mono samples
		out[i] = &model.AudioBlock{
			Seq: uint32(i), TimestampNS: int64(i) * 20_000_000,
			SampleRate: 8000, Channels: 1, Samples: 160, Payload: payload,
		}
	}
	return out
}

func testConfig(seed int64) Config {
	return Config{
		WindowNS:      1_000_000_000,
		SilenceThresh: 4,
		SilenceMin:    160,
		RTP: rtp.Config{
			Seed: seed, LossBurst: 3, BurstEvery: 40,
			ReorderWindow: 3, BufferSlots: MaxBufferedFrames,
		},
	}
}

type memSink struct{ segs []model.Segment }

func (m *memSink) Write(s model.Segment) error {
	m.segs = append(m.segs, s)
	return nil
}

type failSink struct{ after int }

func (f *failSink) Write(s model.Segment) error {
	f.after--
	if f.after <= 0 {
		return errors.New("disk full")
	}
	return nil
}

func TestRunHappyPathMetrics(t *testing.T) {
	r, err := New(testConfig(42))
	if err != nil {
		t.Fatal(err)
	}
	sink := &memSink{}
	m, err := r.Run(context.Background(), buildFrames(240), buildAudio(120), 9, sink)
	if err != nil {
		t.Fatal(err)
	}
	if m.FramesIndexed != 240 {
		t.Fatalf("frames indexed=%d", m.FramesIndexed)
	}
	if m.FramesBufferedPeak > MaxBufferedFrames {
		t.Fatalf("buffer peak %d exceeds cap", m.FramesBufferedPeak)
	}
	if m.TruncatedTailRecovered != 9 {
		t.Fatalf("tail recovered=%d", m.TruncatedTailRecovered)
	}
	if m.PacketRecoveryPct < 95 {
		t.Fatalf("recovery %.2f%% below 95", m.PacketRecoveryPct)
	}
	if m.AVSyncErrorMS > 20 {
		t.Fatalf("av sync %.2f ms exceeds 20", m.AVSyncErrorMS)
	}
	if m.LatencyP95MS > 120 {
		t.Fatalf("p95 %.2f ms exceeds 120", m.LatencyP95MS)
	}
	if m.SilenceIntervals < 1 || m.UniquePayloads != 12 || m.ReusedRefs == 0 {
		t.Fatalf("content metrics wrong: %+v", m)
	}
	if len(sink.segs) != m.Segments || m.Segments == 0 {
		t.Fatalf("segments emitted=%d counted=%d", len(sink.segs), m.Segments)
	}
}

func TestRunDeterministic(t *testing.T) {
	run := func() *Metrics {
		r, _ := New(testConfig(42))
		m, err := r.Run(context.Background(), buildFrames(240), buildAudio(120), 0, &memSink{})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a, b := run(), run()
	if a.Digest != b.Digest || a.PacketRecoveryPct != b.PacketRecoveryPct ||
		a.Segments != b.Segments || a.LatencyP95MS != b.LatencyP95MS {
		t.Fatalf("repeated runs differ:\n%+v\n%+v", a, b)
	}
}

func TestRunCancellationKeepsStats(t *testing.T) {
	r, _ := New(testConfig(1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m, err := r.Run(ctx, buildFrames(240), buildAudio(120), 0, &memSink{})
	if err == nil {
		t.Fatal("cancelled context must error")
	}
	le, ok := model.AsLocatable(err)
	if !ok || le.Code != model.ErrCancelled {
		t.Fatalf("want cancellation error, got %v", err)
	}
	// Stats gathered before the failure remain available.
	if m == nil {
		t.Fatal("metrics must be returned even on cancellation")
	}
}

func TestRunSinkFailureKeepsStats(t *testing.T) {
	r, _ := New(testConfig(1))
	m, err := r.Run(context.Background(), buildFrames(240), buildAudio(120), 0, &failSink{after: 2})
	if err == nil {
		t.Fatal("sink failure must surface")
	}
	le, ok := model.AsLocatable(err)
	if !ok || le.Code != model.ErrWrite {
		t.Fatalf("want write error, got %v", err)
	}
	if m == nil || m.Segments == 0 {
		t.Fatal("partial metrics must survive a sink failure")
	}
}

func TestBufferCapHonored(t *testing.T) {
	r, _ := New(testConfig(7))
	m, err := r.Run(context.Background(), buildFrames(300), nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.FramesBufferedPeak != MaxBufferedFrames {
		t.Fatalf("peak=%d want exactly %d", m.FramesBufferedPeak, MaxBufferedFrames)
	}
}
