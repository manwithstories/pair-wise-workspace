// Package fixture synthesises reproducible RLY1 relay segments for the audit
// pipeline. Every byte is derived from the supplied seed; nothing is read from
// external files.
//
// The generated segment carries four tracks (two H.264 video, two ADTS AAC
// audio) whose PTS base sits shortly before the 32-bit wrap point, so any
// downstream stage that fails to extend the 90 kHz clock will mis-report
// keyframe intervals and clip boundaries.
package fixture

import (
	"fmt"
	"math/rand"

	"relayaudit/internal/clock"
	"relayaudit/internal/wire"
)

// Generate builds the segment described by spec.
func Generate(spec Spec) (*Segment, error) {
	if spec.Size < 1<<16 {
		return nil, fmt.Errorf("fixture size %d is too small", spec.Size)
	}
	base := clock.Wrap - 10*clock.Rate // ten seconds before the 32-bit wrap
	cur := new(mediaCursor)

	// Plan records in PTS order until the segment would exceed Size. Record
	// sizes are exact, so the encoded file lands at or under the budget.
	var plan []paced
	recCount := 0
	for {
		ticks := cur.candidates()
		best := 0
		for i := 1; i < 4; i++ {
			if ticks[i] < ticks[best] {
				best = i
			}
		}
		sz := payloadSize(best, ticks[best]) + wire.RecordOverhead
		pageBytes := wire.PagesFor(recCount+1) * wire.PageSize
		if wire.HeaderSize+pageBytes+totalPayload(plan)+sz > spec.Size {
			break
		}
		plan = append(plan, paced{track: uint8(best + 1), pts: ticks[best], size: sz})
		cur.advance(best)
		recCount++
		if recCount > 1<<20 {
			return nil, fmt.Errorf("fixture generation runaway")
		}
	}
	if recCount < 32 {
		return nil, fmt.Errorf("size %d yields only %d records", spec.Size, recCount)
	}

	pages := wire.PagesFor(recCount)
	dataStart := int64(wire.HeaderSize) + int64(pages)*wire.PageSize
	total := dataStart + int64(totalPayload(plan))
	out := make([]byte, total)

	hdr := wire.EncodeHeader(wire.Header{
		Version: wire.Version, Tracks: 4, RecordCount: uint32(recCount),
		PageCount: uint32(pages), DataStart: dataStart,
	})
	copy(out[:wire.HeaderSize], hdr[:])

	// Emit records and their index entries. Payload bytes are drawn from the
	// seeded RNG in a fixed order, so the whole file is reproducible.
	rnd := rand.New(rand.NewSource(spec.Seed))
	records := make([]*wire.Record, 0, recCount)
	entries := make([]wire.Entry, 0, recCount)
	var seqs [4]uint32
	off := dataStart
	for _, p := range plan {
		ti := int(p.track - 1)
		payload := buildPayload(rnd, ti, p.pts)
		if len(payload)+wire.RecordOverhead != p.size {
			return nil, fmt.Errorf("internal size drift: planned %d got %d", p.size, len(payload)+wire.RecordOverhead)
		}
		rec := &wire.Record{Track: p.track, Seq: seqs[ti], PTS: wireStamp(base, p.pts), Payload: payload, Offset: off}
		buf := rec.Marshal(nil)
		copy(out[off:off+int64(len(buf))], buf)
		rec.CRC = wire.PayloadCRC(payload)
		records = append(records, rec)
		entries = append(entries, wire.Entry{
			Offset: off, Length: uint32(len(buf)), Track: p.track, SeqLow: uint16(seqs[ti]),
		})
		seqs[ti]++
		off += int64(len(buf))
	}
	copy(out[wire.HeaderSize:dataStart], wire.EncodeIndex(entries))

	seg := &Segment{
		Records: records, Entries: entries, TrackCount: 4,
		BaseRaw: clock.Stamp(base & (clock.Wrap - 1)),
	}
	if n := len(records); n > 0 {
		seg.DurationTicks = uint32Delta(uint32(base&(clock.Wrap-1)), uint32(records[n-1].PTS))
	}
	seg.Damage.CleanBytes = len(out)
	damaged, err := applyDamage(out, seg, spec)
	if err != nil {
		return nil, err
	}
	seg.Bytes = damaged
	return seg, nil
}

// wireStamp maps an absolute tick to its 32-bit wire stamp.
func wireStamp(base, ticks uint64) clock.Stamp {
	return clock.Stamp((base + ticks) & (clock.Wrap - 1))
}

// uint32Delta returns the forward 32-bit distance from a to b.
func uint32Delta(a, b uint32) uint64 { return uint64(b - a) }

func totalPayload(plan []paced) int {
	n := 0
	for _, p := range plan {
		n += p.size
	}
	return n
}
