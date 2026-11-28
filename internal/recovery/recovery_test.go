package recovery

import (
	"bytes"
	"testing"

	"archive/internal/ingest"
	"archive/internal/media"
)

func groupParity(base int64, payloads [][]byte) ingest.ParityInfo {
	pi := ingest.ParityInfo{BaseSeq: base, Count: len(payloads), XOR: XorPayloads(payloads)}
	for i, p := range payloads {
		pi.PTS = append(pi.PTS, int64(i)*1000)
		pi.Samples = append(pi.Samples, 1024)
		pi.Lengths = append(pi.Lengths, len(p))
	}
	return pi
}

func audioPkt(seq int64, data []byte) *media.Packet {
	return &media.Packet{Track: 2, Seq: seq, Kind: media.KindAudio, PTS: seq * 1000, Data: data}
}

func TestRecoverSingleLoss(t *testing.T) {
	payloads := [][]byte{{1, 2, 3, 4}, {9, 9}, {5, 6, 7, 8}, {0, 1}}
	pi := groupParity(0, payloads)
	present := []*media.Packet{audioPkt(0, payloads[0]), audioPkt(2, payloads[2]), audioPkt(3, payloads[3])}
	full, rec, lost := RecoverAll(2, present, []ingest.ParityInfo{pi})
	if rec != 1 || lost != 0 {
		t.Fatalf("rec=%d lost=%d", rec, lost)
	}
	got := full[1]
	if got.Lost || !got.Recovered {
		t.Fatalf("packet not marked recovered: %+v", got)
	}
	if !bytes.Equal(got.Data, payloads[1]) {
		t.Fatalf("rebuilt %v, want %v", got.Data, payloads[1])
	}
	if got.PTS != 1000 {
		t.Fatalf("PTS = %d, want 1000 from parity header", got.PTS)
	}
}

func TestMultiLossKeepsGap(t *testing.T) {
	payloads := [][]byte{{1}, {2}, {3}, {4}}
	pi := groupParity(0, payloads)
	present := []*media.Packet{audioPkt(0, payloads[0]), audioPkt(3, payloads[3])}
	full, rec, lost := RecoverAll(2, present, []ingest.ParityInfo{pi})
	if rec != 0 || lost != 2 {
		t.Fatalf("rec=%d lost=%d", rec, lost)
	}
	// Gaps stay in place; later packets are not shifted.
	if !full[1].Lost || !full[2].Lost {
		t.Fatalf("gaps moved: %+v %+v", full[1], full[2])
	}
	if full[3].Seq != 3 || full[3].Lost {
		t.Fatalf("later packet disturbed: %+v", full[3])
	}
}

func TestNoParityCoverage(t *testing.T) {
	present := []*media.Packet{audioPkt(0, []byte{1}), audioPkt(2, []byte{3})}
	full, rec, lost := RecoverAll(2, present, nil)
	if rec != 0 || lost != 1 || !full[1].Lost {
		t.Fatalf("rec=%d lost=%d full=%v", rec, lost, full)
	}
}

func TestXorSymmetry(t *testing.T) {
	a := [][]byte{{0xDE, 0xAD}, {0xBE, 0xEF, 0x01}}
	x := XorPayloads(a)
	// XOR the first payload back out to recover the second.
	for i, b := range a[0] {
		x[i] ^= b
	}
	if !bytes.Equal(x[:len(a[1])], a[1]) {
		t.Fatalf("xor round trip failed: %v", x)
	}
}
