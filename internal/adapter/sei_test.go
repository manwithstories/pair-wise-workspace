package adapter

import (
	"testing"

	"mediaroute/internal/fault"
)

func TestSEIParserBasic(t *testing.T) {
	p := NewSEIParser()
	evs, err := p.Parse("sei", BuildSEI(123_000_000, []byte("tc")), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != KindSEI || evs[0].TimestampNs != 123_000_000 {
		t.Fatalf("bad events: %+v", evs)
	}
	if string(evs[0].Payload) != "tc" {
		t.Fatalf("payload = %q", evs[0].Payload)
	}
}

func TestSEIParserTruncated(t *testing.T) {
	p := NewSEIParser()
	good := BuildSEI(1, []byte("abcdef"))
	if _, err := p.Parse("sei", good[:len(good)-4], 0); err == nil {
		t.Fatal("truncated payload accepted")
	} else if fe := err.(*fault.Error); fe.Category != fault.CatParse {
		t.Fatalf("category = %s", fe.Category)
	}
	if _, err := p.Parse("sei", []byte{0x06}, 0); err == nil {
		t.Fatal("short nal accepted")
	}
	if _, err := p.Parse("sei", []byte{0x05, 0x80}, 0); err == nil {
		t.Fatal("wrong nal type accepted")
	}
}
