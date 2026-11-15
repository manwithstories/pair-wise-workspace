package signal

import (
	"encoding/json"
	"testing"
)

func envelope(seq uint64, source string, messageType MessageType) Envelope {
	return Envelope{
		Version: ProtocolVersion, Seq: seq, Type: messageType,
		Stream: "s", Source: source, Payload: json.RawMessage(`{}`),
	}
}

func TestEnvelopeVersionAndTypeValidation(t *testing.T) {
	env := envelope(1, "peer", TypeOffer)
	data, err := MarshalEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalEnvelope(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Type != TypeOffer || decoded.Seq != 1 {
		t.Fatalf("unexpected envelope: %+v", decoded)
	}
	badVersion := env
	badVersion.Version = ProtocolVersion + 1
	if err := ValidateEnvelope(badVersion); err == nil {
		t.Fatal("expected version validation failure")
	}
	badType := env
	badType.Type = "bogus"
	if err := ValidateEnvelope(badType); err == nil {
		t.Fatal("expected type validation failure")
	}
}

func TestSequenceCacheOrdersFutureAndDeduplicates(t *testing.T) {
	cache := NewSequenceCache()
	ready, accepted := cache.Accept(envelope(1, "peer", TypeOffer))
	if !accepted || len(ready) != 1 || ready[0].Seq != 1 {
		t.Fatalf("first packet: ready=%v accepted=%v", ready, accepted)
	}
	if _, accepted = cache.Accept(envelope(1, "peer", TypeOffer)); accepted {
		t.Fatal("duplicate sequence must be ignored")
	}
	ready, accepted = cache.Accept(envelope(3, "peer", TypeTrickle))
	if !accepted || len(ready) != 0 || cache.Pending() != 1 {
		t.Fatalf("future packet: ready=%v accepted=%v pending=%d", ready, accepted, cache.Pending())
	}
	if _, accepted = cache.Accept(envelope(3, "peer", TypeTrickle)); accepted {
		t.Fatal("duplicate future sequence must be ignored")
	}
	ready, accepted = cache.Accept(envelope(2, "peer", TypeAnswer))
	if !accepted || len(ready) != 2 || ready[0].Seq != 2 || ready[1].Seq != 3 {
		t.Fatalf("gap fill: %+v", ready)
	}
	if cache.Pending() != 0 {
		t.Fatalf("pending should be empty, got %d", cache.Pending())
	}
	if _, accepted = cache.Accept(envelope(2, "peer", TypeAnswer)); accepted {
		t.Fatal("stale sequence must be ignored")
	}
}

func TestSequenceCacheSeparatesSources(t *testing.T) {
	cache := NewSequenceCache()
	if ready, accepted := cache.Accept(envelope(1, "a", TypeOffer)); !accepted || len(ready) != 1 {
		t.Fatal("source a first message")
	}
	if ready, accepted := cache.Accept(envelope(1, "b", TypeAnswer)); !accepted || len(ready) != 1 {
		t.Fatal("source b first message")
	}
}
