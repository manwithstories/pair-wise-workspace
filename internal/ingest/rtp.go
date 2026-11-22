// Package ingest turns raw RTP records into model events on the unified clock.
// It owns payload validation, sequence/timestamp unwrapping and oversize
// rejection; failures are returned already classified with a model.ErrorKind.
package ingest

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"

	"example.com/medialab/archive/internal/clock"
	"example.com/medialab/archive/internal/codec"
	"example.com/medialab/archive/internal/model"
)

// RawRecord is one entry from the local packet log. ArrivalUS is the arrival
// offset used for lateness accounting; it never influences media time.
type RawRecord struct {
	ArrivalUS int64
	Bytes     []byte
}

// StreamConfig freezes everything needed to interpret a stream's records.
type StreamConfig struct {
	ID        model.StreamID
	Rep       model.RepresentationID
	Codec     model.Codec
	ClockRate clock.Rate
	AAC       codec.AACConfig
}

// Validate checks the codec/clock combination.
func (c StreamConfig) Validate() error {
	if c.ID == "" || c.Rep == "" {
		return errors.New("ingest: stream and representation ids are required")
	}
	if !c.Codec.Valid() {
		return fmt.Errorf("ingest: unsupported codec %q", c.Codec)
	}
	if err := c.ClockRate.Validate(); err != nil {
		return err
	}
	want := uint32(codec.OpusRate)
	if c.Codec == model.CodecAAC {
		want = uint32(codec.AACRate)
	}
	if uint32(c.ClockRate) != want {
		return fmt.Errorf("ingest: codec %s requires clock rate %d, got %d", c.Codec, want, c.ClockRate)
	}
	if c.Codec == model.CodecAAC {
		return c.AAC.Validate()
	}
	return nil
}

// Parser is a stateful per-stream record parser. Not safe for concurrent use;
// one parser serves one stream.
type Parser struct {
	cfg    StreamConfig
	tsUnw  clock.TimestampUnwrapper
	seqUnw clock.SequenceUnwrapper
	baseTS bool
	base   int64
	// Counters accumulates the shared ingest counters.
	Counters Counters
}

// Counters holds the mandatory loss/duplicate/reorder accounting.
type Counters struct {
	Total     int64
	Accepted  int64
	Loss      int64
	Duplicate int64
	Reorder   int64
	Corrupt   int64
	Oversize  int64
	Invalid   int64
	MaxSeq    int64
	seenMax   bool
}

// NewParser validates the configuration and returns a parser.
func NewParser(cfg StreamConfig) (*Parser, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Parser{cfg: cfg, Counters: Counters{MaxSeq: -1}}, nil
}

// Config returns the frozen stream configuration.
func (p *Parser) Config() StreamConfig { return p.cfg }

// Parse converts one raw record. A nil event with a nil error means the record
// was a duplicate and must not be counted twice. A non-nil error is always
// classified and the event is nil.
func (p *Parser) Parse(rec RawRecord) (*model.Event, error) {
	p.Counters.Total++
	if len(rec.Bytes) > model.MaxPayloadBytes+256 { // RTP header allowance
		p.Counters.Oversize++
		return nil, model.Classify(model.KindOversize, fmt.Errorf("record of %d bytes exceeds limit", len(rec.Bytes)))
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(rec.Bytes); err != nil {
		p.Counters.Invalid++
		return nil, model.Classify(model.KindInvalidPacket, fmt.Errorf("rtp header: %w", err))
	}
	if len(pkt.Payload) > model.MaxPayloadBytes {
		p.Counters.Oversize++
		return nil, model.Classify(model.KindOversize, fmt.Errorf("payload of %d bytes exceeds 1 MiB", len(pkt.Payload)))
	}
	extSeq := p.seqUnw.Unwrap(pkt.SequenceNumber)
	id := model.PacketID{Stream: p.cfg.ID, Seq: uint32(extSeq)}

	// Loss and reorder are derived from the highest sequence seen; the
	// timeline owns duplicate suppression, so a repeated sequence is counted
	// here but still handed over for idempotent handling.
	if p.Counters.seenMax && extSeq < p.Counters.MaxSeq {
		p.Counters.Reorder++
	}
	expected := p.Counters.MaxSeq + 1
	if p.Counters.seenMax && extSeq > expected {
		p.Counters.Loss += extSeq - expected
	}
	if extSeq > p.Counters.MaxSeq || !p.Counters.seenMax {
		p.Counters.MaxSeq = extSeq
		p.Counters.seenMax = true
	}

	extTS := p.tsUnw.Unwrap(pkt.Timestamp)
	if !p.baseTS {
		p.baseTS = true
		p.base = extTS
	}
	relTicks := extTS - p.base
	eventUS := p.cfg.ClockRate.TicksToUS(relTicks)
	if err := clock.ValidateJump(eventUS); err != nil {
		p.Counters.Invalid++
		return nil, model.Classify(model.KindClockJump, err)
	}

	ev := &model.Event{
		ID:         id,
		Codec:      p.cfg.Codec,
		RTPTime:    pkt.Timestamp,
		ExtTimeUS:  eventUS,
		DurationUS: -1,
		Payload:    pkt.Payload,
		Marker:     pkt.Marker,
		ArrivalUS:  rec.ArrivalUS,
	}
	ev.PayloadSHA = ev.PayloadHash()

	dur, err := p.duration(pkt.Payload)
	if err != nil {
		p.Counters.Corrupt++
		ev.DurationUS = -1
		// The event is still returned so the timeline can quarantine it with
		// full provenance; the classified error travels with it by value.
		return ev, model.Classify(model.KindCorrupt, err)
	}
	if err := clock.ValidateDuration(dur); err != nil {
		p.Counters.Corrupt++
		ev.DurationUS = -1
		return ev, model.Classify(model.KindCorrupt, err)
	}
	ev.DurationUS = dur
	p.Counters.Accepted++
	return ev, nil
}

func (p *Parser) duration(payload []byte) (int64, error) {
	switch p.cfg.Codec {
	case model.CodecOpus:
		return codec.OpusDurationUS(payload)
	case model.CodecAAC:
		return p.cfg.AAC.AACDurationUS(payload)
	default:
		return 0, fmt.Errorf("unsupported codec %q", p.cfg.Codec)
	}
}

// MarkDuplicate records a duplicate for accounting.
func (p *Parser) MarkDuplicate() { p.Counters.Duplicate++ }
