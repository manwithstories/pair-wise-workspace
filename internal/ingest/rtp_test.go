package ingest

import (
	"errors"
	"testing"

	"github.com/pion/rtp"

	"example.com/medialab/archive/internal/codec"
	"example.com/medialab/archive/internal/model"
)

func opusConfig() StreamConfig {
	return StreamConfig{
		ID: "opus-64", Rep: "64k", Codec: model.CodecOpus, ClockRate: codec.OpusRate,
	}
}

func encode(t *testing.T, seq uint16, ts uint32, payload []byte) []byte {
	t.Helper()
	p := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 7}, Payload: payload}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func opus20ms() []byte { return []byte{1<<3 | 0<<1, 0x11, 0x22} }

func TestParseDurationAndWrap(t *testing.T) {
	p, err := NewParser(opusConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Start with 0xFFFFFC00 so two 960-tick steps wrap the timestamp.
	base := uint32(0xFFFFFC00)
	if _, err := p.Parse(RawRecord{ArrivalUS: 0, Bytes: encode(t, 65535, base, opus20ms())}); err != nil {
		t.Fatal(err)
	}
	ev, err := p.Parse(RawRecord{ArrivalUS: 20000, Bytes: encode(t, 0, base+960, opus20ms())})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ExtTimeUS != 20000 || ev.DurationUS != 20000 {
		t.Fatalf("event = %+v", ev)
	}
	if ev.ID.Seq != 65536 {
		t.Fatalf("wrapped seq = %d", ev.ID.Seq)
	}
	if p.Counters.Loss != 0 || p.Counters.Reorder != 0 {
		t.Fatalf("counters = %+v", p.Counters)
	}
}

func TestParseLossReorderDuplicate(t *testing.T) {
	p, _ := NewParser(opusConfig())
	parse := func(seq uint16) {
		if _, err := p.Parse(RawRecord{Bytes: encode(t, seq, uint32(seq)*960, opus20ms())}); err != nil {
			t.Fatal(err)
		}
	}
	parse(10)
	parse(11)
	parse(14) // 12,13 lost
	parse(12) // reorder
	parse(14) // duplicate sequence: still counted, timeline dedups
	if p.Counters.Loss != 2 {
		t.Fatalf("loss = %d", p.Counters.Loss)
	}
	if p.Counters.Reorder != 1 {
		t.Fatalf("reorder = %d", p.Counters.Reorder)
	}
}

func TestParseCorruptAndOversize(t *testing.T) {
	p, _ := NewParser(opusConfig())
	ev, err := p.Parse(RawRecord{Bytes: encode(t, 1, 0, []byte{21<<3 | 2<<1})})
	if err == nil || model.KindOf(err) != model.KindCorrupt || ev == nil || ev.DurationUS != -1 {
		t.Fatalf("corrupt handling: ev=%+v err=%v", ev, err)
	}
	huge := make([]byte, model.MaxPayloadBytes+1)
	if _, err := p.Parse(RawRecord{Bytes: encode(t, 2, 0, huge)}); model.KindOf(err) != model.KindOversize {
		t.Fatalf("oversize err = %v", err)
	}
	if _, err := p.Parse(RawRecord{Bytes: []byte{0x00}}); model.KindOf(err) != model.KindInvalidPacket {
		t.Fatalf("invalid err = %v", err)
	}
}

func TestConfigRejectsBadClock(t *testing.T) {
	cfg := opusConfig()
	cfg.ClockRate = 90000
	if _, err := NewParser(cfg); err == nil {
		t.Fatal("opus at 90k accepted")
	}
	cfg = opusConfig()
	cfg.Codec = "mp3"
	if _, err := NewParser(cfg); err == nil {
		t.Fatal("unsupported codec accepted")
	}
	aac := StreamConfig{ID: "a", Rep: "128k", Codec: model.CodecAAC, ClockRate: codec.AACRate, AAC: codec.DefaultAACConfig()}
	if _, err := NewParser(aac); err != nil {
		t.Fatalf("valid aac rejected: %v", err)
	}
}

func TestClassifiedErrorsSurvive(t *testing.T) {
	_, err := NewParser(StreamConfig{})
	if err == nil || errors.Is(err, model.Classify(model.KindInvalidPacket, nil)) {
		t.Fatalf("config error = %v", err)
	}
}

func TestParserStateContinuation(t *testing.T) {
	base := uint32(0xFFFFF000)
	p, _ := NewParser(opusConfig())
	if _, err := p.Parse(RawRecord{Bytes: encode(t, 65534, base, opus20ms())}); err != nil {
		t.Fatal(err)
	}
	st := p.State()
	ev, err := p.Parse(RawRecord{Bytes: encode(t, 65535, base+960, opus20ms())})
	if err != nil {
		t.Fatal(err)
	}
	want := ev.ExtTimeUS

	// A fresh parser restored from the checkpoint sees the same continuation.
	q, _ := NewParser(opusConfig())
	q.Restore(st)
	ev2, err := q.Parse(RawRecord{Bytes: encode(t, 65535, base+960, opus20ms())})
	if err != nil {
		t.Fatal(err)
	}
	if ev2.ExtTimeUS != want || ev2.ID.Seq != ev.ID.Seq {
		t.Fatalf("restored continuation = %+v want time %d seq %d", ev2, want, ev.ID.Seq)
	}
	// Counters survive too.
	if q.Counters.Total != 2 {
		t.Fatalf("restored total = %d", q.Counters.Total)
	}
}
