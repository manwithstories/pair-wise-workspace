package adapter

import (
	"testing"

	"mediaroute/internal/fault"
)

func TestMP4ParserEmsg(t *testing.T) {
	p := NewMP4Parser()
	box := BuildEmsg(1000, 2500, 500, 1, "urn:test", "v", []byte("hello"))
	data := append(BuildBox("free", []byte("pad")), box...)
	evs, err := p.Parse("meta", data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	ev := evs[0]
	if ev.Kind != KindEmsg || ev.TimestampNs != 2_500_000_000 || ev.DurationNs != 500_000_000 {
		t.Fatalf("bad emsg event: %+v", ev)
	}
	if string(ev.Payload) != "hello" {
		t.Fatalf("payload = %q", ev.Payload)
	}
}

func TestMP4ParserTruncatedBox(t *testing.T) {
	p := NewMP4Parser()
	good := BuildEmsg(1000, 0, 0, 1, "s", "v", nil)
	truncated := append(good[:len(good):len(good)], 0, 0, 0, 20, 'e', 'm', 's') // header cut short
	_, err := p.Parse("meta", truncated, 0)
	fe, ok := err.(*fault.Error)
	if !ok || fe.Category != fault.CatParse {
		t.Fatalf("want parse fault, got %v", err)
	}
	if fe.Offset == 0 {
		t.Fatal("offset not reported")
	}
}

func TestMP4ParserIllegalLength(t *testing.T) {
	p := NewMP4Parser()
	data := []byte{0, 0, 0, 4, 'f', 'r', 'e', 'e'} // size < header
	_, err := p.Parse("meta", data, 0)
	fe, ok := err.(*fault.Error)
	if !ok || fe.Category != fault.CatParse {
		t.Fatalf("want parse fault, got %v", err)
	}
}

func TestMP4ParserTruncatedPayload(t *testing.T) {
	p := NewMP4Parser()
	box := BuildEmsg(1000, 0, 0, 1, "s", "v", []byte("payload-data"))
	_, err := p.Parse("meta", box[:len(box)-3], 0)
	if err == nil {
		t.Fatal("truncated payload accepted")
	}
}
