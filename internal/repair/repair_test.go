package repair

import (
	"testing"

	"github.com/pion/rtp"
)

// mkRTP builds a raw RTP datagram carrying the envelope payload.
func mkRTP(seq uint16, ts uint32, primary []byte, red *Redundant) []byte {
	p := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0x11223344,
			Marker:         true,
		},
		Payload: EncodePayload(primary, red),
	}
	raw, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	return raw
}

func body(seq uint16) []byte {
	return []byte{byte(seq), byte(seq >> 8), 0xAA, 0xBB}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	primary := []byte("primary")
	red := &Redundant{Seq: 7, Bytes: []byte("previous")}
	gotPrimary, gotRed, err := DecodeEnvelope(EncodePayload(primary, red))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotPrimary) != "primary" {
		t.Fatalf("primary=%q", gotPrimary)
	}
	if gotRed == nil || gotRed.Seq != 7 || string(gotRed.Bytes) != "previous" {
		t.Fatalf("redundant=%+v", gotRed)
	}
}

func TestEnvelopeRejectsMalformed(t *testing.T) {
	if _, _, err := DecodeEnvelope([]byte{1, 0, 0}); err == nil {
		t.Fatal("expected error for truncated redundancy header")
	}
	bad := []byte{1, 0, 5, 0xFF}
	if _, _, err := DecodeEnvelope(bad); err == nil {
		t.Fatal("expected error for overlong redundancy length")
	}
}

func TestAnalyzeCleanStream(t *testing.T) {
	var raws [][]byte
	for i := 0; i < 50; i++ {
		raws = append(raws, mkRTP(uint16(i), uint32(i)*3000, body(uint16(i)), nil))
	}
	res := Analyze(raws)
	if res.Lost != 0 || res.Received != 50 || res.Expected != 50 {
		t.Fatalf("res=%+v", res)
	}
	if res.RecoveryPct != 100 {
		t.Fatalf("recovery=%v", res.RecoveryPct)
	}
	if len(res.Payloads) != 50 {
		t.Fatalf("payloads=%d", len(res.Payloads))
	}
}

func TestAnalyzeReordersAndDuplicates(t *testing.T) {
	var raws [][]byte
	for _, i := range []int{0, 1, 3, 2, 4, 4, 5} {
		raws = append(raws, mkRTP(uint16(i), uint32(i)*3000, body(uint16(i)), nil))
	}
	res := Analyze(raws)
	if res.Duplicates != 1 {
		t.Fatalf("duplicates=%d", res.Duplicates)
	}
	if res.Reordered != 1 {
		t.Fatalf("reordered=%d", res.Reordered)
	}
	if res.Lost != 0 {
		t.Fatalf("lost=%d gaps=%v", res.Lost, res.Gaps)
	}
	for i := 1; i < len(res.Payloads); i++ {
		if res.Payloads[i].Seq != res.Payloads[i-1].Seq+1 {
			t.Fatalf("output not in sequence order at %d", i)
		}
	}
}

func TestAnalyzeRecoversFromRedundancy(t *testing.T) {
	var raws [][]byte
	const total = 40
	drop := map[int]bool{10: true, 11: true, 25: true}
	for i := 0; i < total; i++ {
		var red *Redundant
		if i > 0 {
			red = &Redundant{Seq: uint16(i - 1), Bytes: body(uint16(i - 1))}
		}
		if drop[i] {
			continue
		}
		raws = append(raws, mkRTP(uint16(i), uint32(i)*3000, body(uint16(i)), red))
	}
	res := Analyze(raws)
	// A missing packet X is restored from X+1's redundant copy. Packet 10 is
	// lost along with packet 11, which was the only carrier of its copy, so it
	// stays lost; 11 and 25 are recovered from packets 12 and 26.
	if res.Lost != 1 || len(res.Gaps) != 1 || res.Gaps[0].Start != 10 {
		t.Fatalf("lost=%d gaps=%v", res.Lost, res.Gaps)
	}
	if res.Recovered != 2 {
		t.Fatalf("recovered=%d", res.Recovered)
	}
	if got, want := res.RecoveryPct, 2.0/3.0*100; got < want-0.01 || got > want+0.01 {
		t.Fatalf("recovery pct=%v want %v", got, want)
	}
}

func TestAnalyzeReportsUnrecoverable(t *testing.T) {
	var raws [][]byte
	const total = 30
	for i := 0; i < total; i++ {
		if i >= 12 && i <= 15 {
			continue // four consecutive losses, no redundancy survives
		}
		var red *Redundant
		if i > 0 {
			red = &Redundant{Seq: uint16(i - 1), Bytes: body(uint16(i - 1))}
		}
		raws = append(raws, mkRTP(uint16(i), uint32(i)*3000, body(uint16(i)), red))
	}
	res := Analyze(raws)
	// Packet 16 is present and carries a copy of 15, so the run 12-14 is lost.
	if len(res.Gaps) != 1 || res.Gaps[0].Count != 3 {
		t.Fatalf("gaps=%v lost=%d", res.Gaps, res.Lost)
	}
	if res.Gaps[0].Start != 12 || res.Gaps[0].End != 14 {
		t.Fatalf("gap=%+v", res.Gaps[0])
	}
	if res.Recovered != 1 {
		t.Fatalf("recovered=%d", res.Recovered)
	}
	if res.RecoveryPct >= 95 {
		t.Fatalf("recovery should be below 95 for an unrepairable run: %v", res.RecoveryPct)
	}
	if res.LossPct <= 0 {
		t.Fatalf("loss pct=%v", res.LossPct)
	}
}

func TestAnalyzeSequenceWrap(t *testing.T) {
	var raws [][]byte
	for i := 65530; i < 65536; i++ {
		raws = append(raws, mkRTP(uint16(i), uint32(i)*3000, body(uint16(i)), nil))
	}
	for i := 0; i < 10; i++ {
		raws = append(raws, mkRTP(uint16(i), uint32(i)*3000, body(uint16(i)), nil))
	}
	res := Analyze(raws)
	if res.Lost != 0 || res.Expected != 16 {
		t.Fatalf("wrap mishandled: %+v", res)
	}
}

func TestAnalyzeWrapNotCountedAsReorder(t *testing.T) {
	var raws [][]byte
	for i := 65530; i < 65536; i++ {
		raws = append(raws, mkRTP(uint16(i), uint32(i)*3000, body(uint16(i)), nil))
	}
	for i := 0; i < 10; i++ {
		raws = append(raws, mkRTP(uint16(i), uint32(i)*3000, body(uint16(i)), nil))
	}
	res := Analyze(raws)
	if res.Reordered != 0 {
		t.Fatalf("monotonic wrap must not be counted as reordering: %d", res.Reordered)
	}
	if len(res.Payloads) != 16 || res.Gaps != nil {
		t.Fatalf("payloads=%d gaps=%v", len(res.Payloads), res.Gaps)
	}
}

func TestAnalyzeLatePacketCountedOnce(t *testing.T) {
	// Arrival order 0,1,2,5,3,4: packets 3 and 4 are late.
	var raws [][]byte
	for _, i := range []int{0, 1, 2, 5, 3, 4} {
		raws = append(raws, mkRTP(uint16(i), uint32(i)*3000, body(uint16(i)), nil))
	}
	res := Analyze(raws)
	if res.Reordered != 2 {
		t.Fatalf("reordered=%d want 2", res.Reordered)
	}
	if res.Lost != 0 {
		t.Fatalf("lost=%d", res.Lost)
	}
}

func TestAnalyzeCorruptDatagram(t *testing.T) {
	var raws [][]byte
	raws = append(raws, mkRTP(0, 0, body(0), nil))
	raws = append(raws, []byte{0x80, 0x00}) // too short for an RTP header
	raws = append(raws, mkRTP(2, 6000, body(2), nil))
	res := Analyze(raws)
	if res.Corrupt != 1 {
		t.Fatalf("corrupt=%d", res.Corrupt)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning for the corrupt datagram")
	}
}

func TestAnalyzeDeterministicDigest(t *testing.T) {
	var raws [][]byte
	for i := 0; i < 100; i++ {
		if i%13 == 5 {
			continue
		}
		var red *Redundant
		if i > 0 {
			red = &Redundant{Seq: uint16(i - 1), Bytes: body(uint16(i - 1))}
		}
		raws = append(raws, mkRTP(uint16(i), uint32(i)*3000, body(uint16(i)), red))
	}
	a := Analyze(raws)
	b := Analyze(raws)
	if a.Digest != b.Digest {
		t.Fatalf("digest unstable: %s %s", a.Digest, b.Digest)
	}
	if len(a.Digest) != 64 {
		t.Fatalf("digest length %d", len(a.Digest))
	}
}
