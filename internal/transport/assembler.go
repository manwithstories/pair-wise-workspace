package transport

import (
	"encoding/binary"

	"mediaroute/internal/fault"
	"mediaroute/internal/fixture"
)

// SampleDamage links a packet-layer gap to the samples it corrupts.
type SampleDamage struct {
	SampleFirst int64
	SampleLast  int64
	Gap         Gap
}

// Assembler rebuilds complete samples from ordered RTP payloads. It only
// ever emits samples whose every fragment arrived; missing fragments are
// reported as damage and never fabricated.
type Assembler struct {
	partial   map[uint32]*partialSample
	maxSample int
	damaged   map[uint32]bool
}

type partialSample struct {
	index    uint32
	size     int
	chunks   map[uint32][]byte // offset -> data
	gotBytes int
	complete bool
}

// NewAssembler bounds fragment allocations to the declared sample count.
func NewAssembler(expectedSamples int) *Assembler {
	return &Assembler{
		partial:   map[uint32]*partialSample{},
		maxSample: expectedSamples,
		damaged:   map[uint32]bool{},
	}
}

// Ingest takes one ordered RTP payload (without RTP header). It buffers
// fragments and returns a sample index plus true only when that sample has
// just become complete. Out-of-range indexes are rejected.
func (a *Assembler) Ingest(payload []byte) (uint32, bool, *fault.Error) {
	if len(payload) < fixture.PayloadHeaderSize {
		return 0, false, fault.New(fault.CatPacketGap,
			"payload shorter than custom header")
	}
	sampleIdx := binary.BigEndian.Uint32(payload[0:])
	offset := binary.BigEndian.Uint32(payload[4:])
	sampleSize := int(binary.BigEndian.Uint16(payload[8:]))
	flags := binary.BigEndian.Uint16(payload[12:])
	body := payload[fixture.PayloadHeaderSize:]
	if int(sampleIdx) >= a.maxSample {
		return 0, false, fault.New(fault.CatLimitExceeded,
			"sample index %d beyond declared %d", sampleIdx, a.maxSample)
	}
	if a.damaged[sampleIdx] {
		return 0, false, nil
	}
	ps := a.partial[sampleIdx]
	if ps == nil {
		ps = &partialSample{index: sampleIdx, size: sampleSize, chunks: map[uint32][]byte{}}
		a.partial[sampleIdx] = ps
	}
	if _, dup := ps.chunks[offset]; !dup {
		ps.chunks[offset] = body
		ps.gotBytes += len(body)
	}
	if flags&fixture.FlagLast != 0 {
		ps.complete = true
	}
	if ps.complete && ps.gotBytes >= ps.size {
		return sampleIdx, true, nil
	}
	return 0, false, nil
}

// Materialize concatenates the fragments of a complete sample.
func (a *Assembler) Materialize(sampleIdx uint32) ([]byte, bool) {
	ps := a.partial[sampleIdx]
	if ps == nil || !ps.complete || ps.gotBytes < ps.size {
		return nil, false
	}
	out := make([]byte, ps.size)
	for off, chunk := range ps.chunks {
		copy(out[off:], chunk)
	}
	delete(a.partial, sampleIdx)
	return out, true
}

// MarkDamage records samples touched by an expired gap. It maps the gap's
// packet sequence range to every sample that had a fragment in flight.
func (a *Assembler) MarkDamage(g Gap) SampleDamage {
	first, last := a.gapSampleRange(g)
	for s := first; s <= last; s++ {
		a.damaged[uint32(s)] = true
		delete(a.partial, uint32(s))
	}
	return SampleDamage{SampleFirst: first, SampleLast: last, Gap: g}
}

// gapSampleRange infers the affected samples from whatever fragments are
// buffered at the moment the gap expires. When the gap covers samples that
// never started, the caller-provided track index mapping refines the range.
func (a *Assembler) gapSampleRange(g Gap) (int64, int64) {
	lo := int64(-1)
	hi := int64(-1)
	for idx := range a.partial {
		i := int64(idx)
		if lo < 0 || i < lo {
			lo = i
		}
		if i > hi {
			hi = i
		}
	}
	if lo < 0 {
		// No partial samples: attribute the gap to a single placeholder so
		// the engine can refine it via the track time-range mapping.
		base := int64(g.ExtFirst)
		return base, base
	}
	return lo, hi
}

// DamagedCount returns how many distinct samples were marked corrupt.
func (a *Assembler) DamagedCount() int { return len(a.damaged) }

// IsDamaged reports whether a sample was touched by packet loss.
func (a *Assembler) IsDamaged(sampleIdx int64) bool {
	return a.damaged[uint32(sampleIdx)]
}

// DamageFault builds the sample-level fault, carrying both sample and
// packet locators plus the shared time range.
func (d SampleDamage) DamageFault() *fault.Error {
	return fault.New(fault.CatSampleDamage,
		"samples %d..%d unrecoverable from packet seq %d..%d",
		d.SampleFirst, d.SampleLast, d.Gap.SeqFirst, d.Gap.SeqLast).
		WithSample(d.SampleFirst).
		WithSeq(int64(d.Gap.ExtFirst), int64(d.Gap.ExtLast)).
		WithTimeRange(d.Gap.TimeRange)
}
