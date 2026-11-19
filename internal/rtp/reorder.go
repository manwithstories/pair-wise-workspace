// Package rtp reconstructs RTP stream state from a lossy capture: it removes
// duplicates, reorders within a bounded receive window, and classifies packets
// that arrive too late to fit the window as unrecoverable loss.
package rtp

import (
	"fmt"
	"sort"

	"archivearchivist/internal/model"
)

// ExtIDTimestamp carries the full 33-bit transport clock in an RTP header
// extension, because the RFC 3550 timestamp field is only 32 bits. Receivers
// that lack the extension fall back to the 32-bit field and can still unwrap
// indefinitely many 32-bit cycles, but not a 33-bit clock.
const ExtIDTimestamp uint8 = 3

// extensionBytesLen is the encoded length stored in one extension byte.
const extensionBytesLen = 8

// Packet is one decoded capture record.
type Packet struct {
	Seq       uint16
	Timestamp uint32
	// ExtTimestamp is the sender's extended clock in the track's own units.
	// RTP carries only 32 bits, so a 33-bit transport clock is recorded
	// alongside it in the short extension block the fixture generator emits.
	ExtTimestamp int64
	HasExt       bool
	SSRC         uint32
	Marker       bool
	Payload      []byte
	ArrivalUS    int64
}

// Options configures reordering.
type Options struct {
	// Window is the number of sequence numbers a late packet may trail the
	// high-water mark and still be accepted. Zero means 64 (32..4095).
	Window int
	// Track is used only to label faults.
	Track string
}

// Outcome classifies one packet after window processing.
type Outcome string

const (
	OutcomeAccepted  Outcome = "accepted"
	OutcomeDuplicate Outcome = "duplicate"
	OutcomeLate      Outcome = "late"
)

// Report is the ordered stream plus loss accounting.
type Report struct {
	Stream        []Packet
	Received      int
	Accepted      int
	Duplicates    int
	OutOfOrder    int
	Late          int
	Missing       int
	FirstSeq      uint16
	LastSeq       uint16
	MissingSeqs   []uint32
	OutOfOrderSeq []uint32
	LateSeqs      []uint32
	Faults        model.FaultList
}

// RecoveryRate is the share of the expected sequence span that was recovered
// in presentation order: accepted / (accepted + missing).
func (r Report) RecoveryRate() model.Rational {
	total := r.Accepted + r.Missing
	if total == 0 {
		return model.NewRat(1, 1)
	}
	return model.NewRat(int64(r.Accepted), int64(total))
}

// Expected is the number of sequence numbers between first and last inclusive.
func (r Report) Expected() int { return int(r.LastSeq) - int(r.FirstSeq) + 1 }

// Reorder consumes packets in capture order and returns them in sequence
// order. Sequence numbers are unwrapped incrementally into a 64-bit extended
// space, so a capture spanning many 16-bit cycles is handled without overflow
// and with a deterministic result.
func Reorder(packets []Packet, opts Options) Report {
	if opts.Window <= 0 {
		opts.Window = 64
	}
	rep := Report{Received: len(packets)}
	if len(packets) == 0 {
		rep.Faults.Add(model.NewFault(model.CodePacketLoss, model.SevWarn, "capture contains no packets"))
		return rep
	}

	var base uint16
	var high int64 // highest extended sequence accepted so far
	seen := make(map[int64]Packet, len(packets))
	lateSeen := make(map[int64]bool)

	for i, p := range packets {
		if i == 0 {
			base = p.Seq
			high = 0
			rep.FirstSeq = p.Seq
			seen[0] = p
			continue
		}
		// The signed 16-bit delta to the current high-water mark places this
		// packet in the extended space; accumulated, it survives any number of
		// 16-bit cycles.
		ext := high + int64(int16(p.Seq-seqAt(base, high)))
		if _, ok := seen[ext]; ok {
			rep.Duplicates++
			continue
		}
		if lateSeen[ext] {
			// A retransmission of a packet already written off as late.
			rep.Duplicates++
			continue
		}
		switch {
		case ext > high:
			seen[ext] = p
			high = ext
		case ext >= high-int64(opts.Window):
			// Behind the mark but still inside the reorder window.
			rep.OutOfOrder++
			rep.OutOfOrderSeq = append(rep.OutOfOrderSeq, uint32(p.Seq))
			seen[ext] = p
		default:
			// Further behind than the window can hold: unrecoverable.
			rep.Late++
			rep.LateSeqs = append(rep.LateSeqs, uint32(p.Seq))
			lateSeen[ext] = true
		}
	}

	// The extended space runs from the lowest accepted packet to the
	// high-water mark; anything in between that was never accepted is a gap.
	exts := make([]int64, 0, len(seen))
	for ext := range seen {
		exts = append(exts, ext)
	}
	sort.Slice(exts, func(i, j int) bool { return exts[i] < exts[j] })
	stream := make([]Packet, 0, len(exts))
	missing := make([]uint32, 0, 8)
	for _, ext := range exts {
		stream = append(stream, seen[ext])
	}
	minExt := exts[0]
	for ext := minExt; ext <= high; ext++ {
		if _, ok := seen[ext]; ok {
			continue
		}
		missing = append(missing, uint32(seqAt(base, ext)))
	}
	rep.Stream = stream
	rep.Accepted = len(stream)
	rep.MissingSeqs = missing
	rep.Missing = len(missing)
	rep.FirstSeq = seqAt(base, minExt)
	rep.LastSeq = seqAt(base, high)

	if rep.Duplicates > 0 {
		rep.Faults.Add(model.Fault{
			Code:     model.CodeDuplicatePacket,
			Severity: model.SevInfo,
			Track:    opts.Track,
			Value:    int64(rep.Duplicates),
			Detail:   fmt.Sprintf("%d duplicate packets discarded", rep.Duplicates),
		})
	}
	if rep.OutOfOrder > 0 {
		rep.Faults.Add(model.Fault{
			Code:     model.CodeOutOfOrder,
			Severity: model.SevInfo,
			Track:    opts.Track,
			Value:    int64(rep.OutOfOrder),
			Detail:   fmt.Sprintf("%d packets reordered within window %d", rep.OutOfOrder, opts.Window),
		})
	}
	if rep.Late > 0 {
		rep.Faults.Add(model.Fault{
			Code:     model.CodeLatePacket,
			Severity: model.SevWarn,
			Track:    opts.Track,
			Value:    int64(rep.Late),
			Detail:   fmt.Sprintf("%d packets arrived outside the %d packet window and are unrecoverable", rep.Late, opts.Window),
		})
	}
	if rep.Missing > 0 {
		detail := fmt.Sprintf("%d packets absent from the capture", rep.Missing)
		sev := model.SevWarn
		if rate := rep.RecoveryRate(); rate.Cmp(model.NewRat(9, 10)) < 0 {
			detail += fmt.Sprintf("; recovery rate %s below 0.90", rate.String())
			sev = model.SevError
		}
		rep.Faults.Add(model.Fault{
			Code:      model.CodePacketLoss,
			Severity:  sev,
			Track:     opts.Track,
			Value:     int64(rep.Missing),
			ByteStart: int64(rep.MissingSeqs[0]),
			ByteEnd:   int64(rep.MissingSeqs[len(rep.MissingSeqs)-1]),
			Detail:    detail,
		})
	}
	rep.Faults.Sort()
	return rep
}

// seqAt maps an extended sequence number back to a 16-bit value.
func seqAt(base uint16, ext int64) uint16 { return base + uint16(ext) }
