// Package ingest converges RTP trajectories into deduplicated, conflict-aware,
// wrap-safe packet order and explicit sequence gaps.
package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/pion/rtp"

	"github.com/pairwise/mediad/internal/media"
)

// Packet is the transport-independent media packet retained by the pipeline.
type Packet struct {
	Sequence     uint16 `json:"sequence"`
	UnwrappedSeq uint64 `json:"unwrapped_sequence"`
	Timestamp    uint32 `json:"timestamp"`
	UnwrappedTS  uint64 `json:"unwrapped_timestamp"`
	SSRC         uint32 `json:"ssrc"`
	Marker       bool   `json:"marker"`
	Payload      []byte `json:"-"`
	ArrivalIndex int    `json:"arrival_index"`
}

// FromPion converts a pion RTP packet and assigns wrap-extended counters.
func FromPion(p *rtp.Packet, lastSeq, lastTS uint64, have bool) Packet {
	seq := media.UnwrapUint(16, lastSeq, uint64(p.SequenceNumber), have)
	ts := media.UnwrapUint(32, lastTS, uint64(p.Timestamp), have)
	return Packet{
		Sequence:     p.SequenceNumber,
		UnwrappedSeq: seq,
		Timestamp:    p.Timestamp,
		UnwrappedTS:  ts,
		SSRC:         p.SSRC,
		Marker:       p.Marker,
		Payload:      append([]byte(nil), p.Payload...),
	}
}

// ToPion converts back to a pion packet for standards-based serialization.
func (p Packet) ToPion() *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        false,
			Marker:         p.Marker,
			PayloadType:    96,
			SequenceNumber: p.Sequence,
			Timestamp:      p.Timestamp,
			SSRC:           p.SSRC,
		},
		Payload: append([]byte(nil), p.Payload...),
	}
}

// MarshalRTP serializes a packet using pion.
func MarshalRTP(p Packet) ([]byte, error) {
	raw, err := p.ToPion().Marshal()
	if err != nil {
		return nil, media.Wrap(err, media.CodeMalformed, "ingest.rtp", "packet", "RTP marshal failed")
	}
	return raw, nil
}

// ParseRTP parses and validates one raw RTP datagram.
func ParseRTP(raw []byte) (*rtp.Packet, error) {
	var p rtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		return nil, media.Wrap(err, media.CodeMalformed, "ingest.rtp", "rtp", "invalid RTP packet")
	}
	return &p, nil
}

// Conflict describes two different payloads claiming one RTP sequence.
type Conflict struct {
	Sequence     uint16 `json:"sequence"`
	UnwrappedSeq uint64 `json:"unwrapped_sequence"`
	FirstHash    string `json:"first_hash"`
	SecondHash   string `json:"second_hash"`
}

func (c Conflict) Error() string {
	return fmt.Sprintf("RTP sequence %d payload conflict %s != %s", c.Sequence, c.FirstHash, c.SecondHash)
}

// Gap is an inclusive missing unwrapped-sequence interval.
type Gap struct {
	From  uint64 `json:"from"`
	To    uint64 `json:"to"`
	Count int    `json:"count"`
}

// Report is the deterministic result of RTP convergence.
type Report struct {
	Packets     []Packet   `json:"-"`
	Duplicates  int        `json:"duplicates"`
	Conflicts   []Conflict `json:"conflicts"`
	Gaps        []Gap      `json:"gaps"`
	Missing     []uint64   `json:"missing"`
	BurstLosses int        `json:"burst_losses"`
	Recovered   int        `json:"recovered"`
	WindowSize  int        `json:"window_size"`
}

// Stream deduplicates and reorders one RTP SSRC/track.
type Stream struct {
	Window      int
	packets     map[uint64]Packet
	payloadHash map[uint64]string
	maxSeq      uint64
	maxTS       uint64
	haveLast    bool
	arrival     int
	duplicates  int
	conflicts   []Conflict
	recovered   int
}

// NewStream creates a bounded reorder buffer.
func NewStream(window int) *Stream {
	if window <= 0 {
		window = 64
	}
	return &Stream{Window: window, packets: make(map[uint64]Packet), payloadHash: make(map[uint64]string)}
}

func hashPayload(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// AcceptRaw parses a raw RTP datagram and accepts it into the stream.
func (s *Stream) AcceptRaw(raw []byte) error {
	p, err := ParseRTP(raw)
	if err != nil {
		return err
	}
	return s.AcceptPion(p)
}

// AcceptPion wraps and accepts a pion packet.
func (s *Stream) AcceptPion(p *rtp.Packet) error {
	pkt := FromPion(p, s.maxSeq, s.maxTS, s.haveLast)
	return s.Accept(pkt)
}

// Accept inserts one packet. Exact duplicates count once; same sequence with a
// different payload is a structured conflict and does not replace the first.
func (s *Stream) Accept(p Packet) error {
	if s.haveLast {
		p.UnwrappedSeq = media.UnwrapUint(16, s.maxSeq, uint64(p.Sequence), true)
		p.UnwrappedTS = media.UnwrapUint(32, s.maxTS, uint64(p.Timestamp), true)
	} else {
		p.UnwrappedSeq = uint64(p.Sequence)
		p.UnwrappedTS = uint64(p.Timestamp)
	}
	p.ArrivalIndex = s.arrival
	s.arrival++
	h := hashPayload(p.Payload)
	if oldHash, ok := s.payloadHash[p.UnwrappedSeq]; ok {
		if oldHash == h {
			s.duplicates++
			return nil
		}
		c := Conflict{Sequence: p.Sequence, UnwrappedSeq: p.UnwrappedSeq, FirstHash: oldHash, SecondHash: h}
		s.conflicts = append(s.conflicts, c)
		return media.NewError(media.CodePacketConflict, "ingest.rtp", "sequence", c.Error())
	}
	s.packets[p.UnwrappedSeq] = p
	s.payloadHash[p.UnwrappedSeq] = h
	if !s.haveLast || p.UnwrappedSeq > s.maxSeq {
		s.maxSeq = p.UnwrappedSeq
	}
	if !s.haveLast || p.UnwrappedTS > s.maxTS {
		s.maxTS = p.UnwrappedTS
	}
	s.haveLast = true
	return nil
}

// Retransmit inserts a packet that fills a previously missing sequence. It is
// not counted as a duplicate because the original was never accepted.
func (s *Stream) Retransmit(p Packet) error {
	if _, ok := s.payloadHash[p.UnwrappedSeq]; ok {
		return s.Accept(p)
	}
	s.recovered++
	return s.Accept(p)
}

// Flush returns sorted unique packets and explicit gap intervals.
func (s *Stream) Flush() Report {
	seqs := make([]uint64, 0, len(s.packets))
	for seq := range s.packets {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var out []Packet
	var missing []uint64
	var gaps []Gap
	if len(seqs) > 0 {
		first, last := seqs[0], seqs[len(seqs)-1]
		present := make(map[uint64]bool, len(seqs))
		for _, seq := range seqs {
			present[seq] = true
			out = append(out, s.packets[seq])
		}
		for seq := first; seq <= last; seq++ {
			if !present[seq] {
				missing = append(missing, seq)
			}
		}
		for i := 0; i < len(missing); {
			j := i + 1
			for j < len(missing) && missing[j] == missing[j-1]+1 {
				j++
			}
			gaps = append(gaps, Gap{From: missing[i], To: missing[j-1], Count: j - i})
			i = j
		}
	}
	return Report{
		Packets:     out,
		Duplicates:  s.duplicates,
		Conflicts:   append([]Conflict(nil), s.conflicts...),
		Gaps:        gaps,
		Missing:     missing,
		BurstLosses: len(gaps),
		Recovered:   s.recovered,
		WindowSize:  s.Window,
	}
}

// EqualPayload is a small helper used by tests and repair validation.
func EqualPayload(a, b []byte) bool { return bytes.Equal(a, b) }
