package rtp

import (
	"bytes"
	"encoding/binary"
	"testing"

	prtp "github.com/pion/rtp"

	"mediaaudit/internal/model"
)

func TestSequenceAndTimestampWrap(t *testing.T) {
	s := NewStream("vo", 90000)
	// Start near the top of both counters and walk forward through a wrap.
	ts := uint32(tsMod - 3)
	seq := uint16(seqMod - 2)
	var prevExtTS, prevExtSeq int64
	for i := 0; i < 6; i++ {
		o := s.Push(seq, ts, int64(i)*1_000_000)
		if i > 0 {
			if o.ExtendedTS-prevExtTS != 5 {
				t.Fatalf("step %d extended TS delta=%d want 5", i, o.ExtendedTS-prevExtTS)
			}
			if o.ExtendedSeq-prevExtSeq != 1 {
				t.Fatalf("step %d extended seq delta=%d want 1", i, o.ExtendedSeq-prevExtSeq)
			}
			if o.Undecidable {
				t.Fatalf("small step must stay decidable at %d", i)
			}
		}
		prevExtTS = o.ExtendedTS
		prevExtSeq = o.ExtendedTS
		prevExtSeq = o.ExtendedSeq
		ts += 5
		seq++
	}
	if s.Faults().Count() != 0 {
		t.Fatalf("unexpected faults: %+v", s.Faults().All())
	}
}

func TestHalfWrapMarkedUndecidable(t *testing.T) {
	s := NewStream("vo", 90000)
	s.Push(0, 1000, 0)
	// A forward step of exactly half the modulus is at the ambiguity boundary.
	o := s.Push(1, uint32(1000+halfWrap), 1_000_000)
	if !o.Undecidable {
		t.Fatal("jump beyond half wrap must be undecidable")
	}
	if !s.Faults().Has(model.KindClockUndecidable) {
		t.Fatal("expected a recorded undecidable fault")
	}
}

// generateDrift builds wall-clock / device-timestamp pairs whose device clock
// runs ppm parts-per-million fast, starting near the wrap boundary.
func generateDrift(t *testing.T, startTS uint32, clockRate uint32, ppm float64, n int) *Stream {
	t.Helper()
	s := NewStream("vo", clockRate)
	slope := 1 + ppm/1e6
	var baseDev int64 = int64(startTS)
	var stepWallNs int64 = 10_000_000 // 10 ms wall clock per packet
	for i := 0; i < n; i++ {
		wall := int64(i) * stepWallNs
		// Device elapsed ticks derive from cumulative wall time so per-packet
		// rounding does not mask the slope.
		devElapsed := slope*float64(wall)/1e9*float64(clockRate) + 0.5
		raw := uint32(baseDev + int64(devElapsed))
		seq := uint16(int64(i) % seqMod)
		o := s.Push(seq, raw, wall)
		if i > 0 && o.Undecidable {
			t.Fatalf("packet %d unexpectedly undecidable (normal drift step)", i)
		}
	}
	return s
}

func TestDriftEstimate180PPM(t *testing.T) {
	// Start 296 ticks below the 32-bit boundary so the stream wraps many times.
	start := uint32(4294967000)
	s := generateDrift(t, start, 90000, 180, 4000)
	res, err := s.EstimateDrift()
	if err != nil {
		t.Fatal(err)
	}
	if res.Undecidable {
		t.Fatal("drift should be decidable with many small steps")
	}
	diff := res.PPM - 180
	if diff < -25 || diff > 25 {
		t.Fatalf("drift PPM=%.2f, want ~180", res.PPM)
	}
}

func TestReadPacketsRoundTrip(t *testing.T) {
	pkt := &prtp.Packet{Header: prtp.Header{SequenceNumber: 7, Timestamp: 12345, SSRC: 9, PayloadType: 96}, Payload: []byte{1, 2, 3}}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := func(wall int64, b []byte) {
		var wb [8]byte
		var lb [4]byte
		binary.LittleEndian.PutUint64(wb[:], uint64(wall))
		binary.LittleEndian.PutUint32(lb[:], uint32(len(b)))
		buf.Write(wb[:])
		buf.Write(lb[:])
		buf.Write(b)
	}
	enc(111, raw)
	enc(222, raw)
	count := 0
	err = ReadPackets(&buf, func(p *prtp.Packet, wall int64) error {
		if p.SequenceNumber != 7 || p.Timestamp != 12345 {
			t.Fatalf("bad packet: %+v", p.Header)
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("decoded %d packets, want 2", count)
	}
}
