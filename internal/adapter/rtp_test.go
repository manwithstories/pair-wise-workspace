package adapter

import (
	"testing"

	"mediaroute/internal/fault"
)

func TestRTPParserBasic(t *testing.T) {
	p := NewRTPParser(90000)
	raw, err := MarshalPacket(7, 90000, true, []byte{0x1, 0xAA})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := p.Parse("video-720p", raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	ev := evs[0]
	if ev.Kind != KindMedia || ev.Seq != 7 || !ev.Keyframe || ev.TimestampNs != 1_000_000_000 {
		t.Fatalf("bad event: %+v", ev)
	}
}

func TestRTPParserWraparound(t *testing.T) {
	p := NewRTPParser(90000)
	mk := func(seq uint16, ts uint32) []byte {
		raw, err := MarshalPacket(seq, ts, false, []byte{0x0})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if _, err := p.Parse("v", mk(1, 0xFFFFFFF0), 0); err != nil {
		t.Fatal(err)
	}
	evs, err := p.Parse("v", mk(2, 0x20), 0)
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].TimestampNs <= 0 {
		t.Fatalf("wrap not extended: %d", evs[0].TimestampNs)
	}
}

func TestRTPParserCorrupt(t *testing.T) {
	p := NewRTPParser(90000)
	_, err := p.Parse("v", []byte{0x80, 0x60, 0x01}, 64)
	fe, ok := err.(*fault.Error)
	if !ok {
		t.Fatalf("want *fault.Error, got %T", err)
	}
	if fe.Category != fault.CatParse || fe.Offset != 64 || fe.Track != "v" {
		t.Fatalf("bad fault: %+v", fe)
	}
}

func TestRegistryVersionRules(t *testing.T) {
	r := NewRegistry()
	r.Register("rtp", NewRTPParser(90000))
	if _, err := r.Parse("nope", "t", []byte{1}, 0); err == nil {
		t.Fatal("unknown parser accepted")
	} else if fe := err.(*fault.Error); fe.Category != fault.CatVersion {
		t.Fatalf("unknown parser category = %s", fe.Category)
	}
	r.Register("bad", stubParser{major: 2})
	if _, err := r.Parse("bad", "t", []byte{1}, 8); err == nil {
		t.Fatal("incompatible major accepted")
	} else if fe := err.(*fault.Error); fe.Category != fault.CatVersion || fe.Offset != 8 {
		t.Fatalf("version fault wrong: %+v", fe)
	}
	if _, err := r.Parse("rtp", "t", make([]byte, MaxPacketSize+1), 0); err == nil {
		t.Fatal("oversize input accepted")
	}
}

type stubParser struct{ major int }

func (s stubParser) MajorVersion() int { return s.major }
func (s stubParser) Parse(string, []byte, int64) ([]Event, error) {
	return nil, nil
}
