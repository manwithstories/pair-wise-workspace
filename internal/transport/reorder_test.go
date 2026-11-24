package transport

import (
	"bytes"
	"testing"

	"mediac/internal/core"
)

// pushRecord wraps a packet as it would appear in a capture frame.
func pushRecord(rec *Reconstructor, rep string, frag int, seq uint16, off uint32, payload []byte) {
	pkt, err := NewPacket(seq, frag, off, payload)
	if err != nil {
		panic(err)
	}
	_ = rec.Push(RecordPacket{
		RepID: rep, Fragment: frag,
		Packet: pkt,
		Offset: off,
	})
}

func TestReconstructClean(t *testing.T) {
	rec := core.NewRecorder()
	r := NewReconstructor(ReorderConfig{Window: 8}, rec)
	pushRecord(r, "v0", 1, 10, 0, []byte("AAAA"))
	pushRecord(r, "v0", 1, 11, 4, []byte("BBBB"))
	pushRecord(r, "v0", 1, 12, 8, []byte("CCCC"))
	got := r.Results()
	if len(got) != 1 || !got[0].Complete || string(got[0].Data) != "AAAABBBBCCCC" {
		t.Fatalf("clean reassembly wrong: %+v", got)
	}
	if rec.HasDamaged() {
		t.Fatal("clean stream must not record damage")
	}
}

func TestReconstructDuplicate(t *testing.T) {
	rec := core.NewRecorder()
	r := NewReconstructor(ReorderConfig{Window: 8}, rec)
	pushRecord(r, "v0", 1, 0, 0, []byte("AA"))
	pushRecord(r, "v0", 1, 0, 0, []byte("AA")) // exact duplicate
	pushRecord(r, "v0", 1, 1, 2, []byte("BB"))
	got := r.Results()
	if !got[0].Complete || string(got[0].Data) != "AABB" {
		t.Fatalf("duplicate handling wrong: %+v", got[0])
	}
	kinds := faultKinds(rec.All())
	if kinds[core.FaultDuplicatePacket] == 0 {
		t.Fatalf("expected duplicate fault, got %+v", kinds)
	}
}

func TestReconstructReordered(t *testing.T) {
	rec := core.NewRecorder()
	r := NewReconstructor(ReorderConfig{Window: 8}, rec)
	pushRecord(r, "v0", 1, 0, 0, []byte("AA"))
	pushRecord(r, "v0", 1, 2, 4, []byte("CC")) // gap at 1, within window
	pushRecord(r, "v0", 1, 1, 2, []byte("BB")) // late but inside window
	got := r.Results()
	if !got[0].Complete || string(got[0].Data) != "AABBCC" {
		t.Fatalf("reorder recovery wrong: %+v", got[0])
	}
	if faultKinds(rec.All())[core.FaultReorderedPacket] == 0 {
		t.Fatal("reordered packet must be reported")
	}
}

func TestReconstructBurstLoss(t *testing.T) {
	rec := core.NewRecorder()
	r := NewReconstructor(ReorderConfig{Window: 4}, rec)
	pushRecord(r, "v0", 1, 0, 0, []byte("AA"))
	// seq 1..5 dropped; seq 6 arrives beyond the window.
	pushRecord(r, "v0", 1, 6, 12, []byte("GG"))
	got := r.Results()
	if got[0].Complete {
		t.Fatal("fragment with burst loss must be incomplete")
	}
	if len(got[0].Gaps) != 1 || got[0].Gaps[0].FirstMissing != 1 || got[0].Gaps[0].LastMissing != 5 {
		t.Fatalf("gap bounds wrong: %+v", got[0].Gaps)
	}
	if faultKinds(rec.All())[core.FaultBurstLoss] == 0 {
		t.Fatal("burst loss fault missing")
	}
}

func TestReconstructLatePacket(t *testing.T) {
	rec := core.NewRecorder()
	r := NewReconstructor(ReorderConfig{Window: 2}, rec)
	pushRecord(r, "v0", 1, 0, 0, []byte("AA"))
	pushRecord(r, "v0", 1, 4, 8, []byte("EE")) // dist 3 > window: forces loss of 1..3
	pushRecord(r, "v0", 1, 1, 2, []byte("BB")) // now older than frontier -> late
	got := r.Results()
	if got[0].Complete {
		t.Fatal("late packet cannot restore completeness")
	}
	if faultKinds(rec.All())[core.FaultLatePacket] == 0 {
		t.Fatal("late packet must be reported as damaged, not silently used")
	}
	if faultKinds(rec.All())[core.FaultBurstLoss] == 0 {
		t.Fatal("the window overflow must also raise a burst loss")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	rep := "v-800"
	var frames [][]byte
	for seq := uint16(0); seq < 4; seq++ {
		pkt, err := NewPacket(seq, 7, uint32(seq)*3,
			[]byte{byte('a' + seq), byte('b' + seq), byte('c' + seq)})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := pkt.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		frame, err := EncodeFrame(rep, 7, uint32(seq+1)*100, raw)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	pkts, err := ReadRecord(bytes.NewReader(bytes.Join(frames, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 4 {
		t.Fatalf("packets=%d", len(pkts))
	}
	if pkts[2].RepID != rep || pkts[2].Fragment != 7 || pkts[2].Offset != 6 {
		t.Fatalf("decoded header wrong: %+v", pkts[2])
	}
	if string(pkts[3].Packet.Payload) != "def" {
		t.Fatalf("payload wrong: %q", pkts[3].Packet.Payload)
	}
}

func TestRecordRejectsTruncation(t *testing.T) {
	pkt, err := NewPacket(0, 1, 0, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	good, err := EncodeFrame("v0", 1, 1, mustMarshal(t, pkt))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(bytes.NewReader(good[:len(good)-3])); err == nil {
		t.Fatal("truncated frame must be rejected")
	}
	if _, err := ReadRecord(bytes.NewReader([]byte("XXXX00"))); err == nil {
		t.Fatal("bad magic must be rejected")
	}
}

func faultKinds(fs []core.Fault) map[core.FaultKind]int {
	out := map[core.FaultKind]int{}
	for _, f := range fs {
		out[f.Kind]++
	}
	return out
}

type marshaler interface {
	Marshal() ([]byte, error)
}

func mustMarshal(t *testing.T, p marshaler) []byte {
	t.Helper()
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
