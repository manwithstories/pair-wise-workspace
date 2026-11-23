package rtprecover

import (
	"testing"

	"mediasync/internal/fixtures"
	"mediasync/internal/model"
)

func build(t *testing.T, o fixtures.RTPOptions) (*fixtures.RTPStream, []Result) {
	t.Helper()
	st, err := fixtures.BuildRTP(o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Recover(st.Packets)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return st, res
}

func TestRecoverCleanStream(t *testing.T) {
	st, res := build(t, fixtures.DefaultRTPOptions())
	if len(res) != 1 {
		t.Fatalf("ssrc count = %d", len(res))
	}
	r := res[0]
	if r.SSRC != st.SSRC {
		t.Fatalf("ssrc = %08x", r.SSRC)
	}
	if r.Duplicate != 0 || r.OutOfOrder != 0 || len(r.Gaps) != 0 {
		t.Fatalf("unexpected impairments: %+v", r)
	}
	var samples int
	for _, p := range r.Packets {
		if !p.Duplicate {
			samples++
		}
	}
	if samples != len(st.ExpectedSamples) {
		t.Fatalf("samples = %d, want %d", samples, len(st.ExpectedSamples))
	}
	for i, p := range r.Packets {
		if string(p.Payload) != string(st.ExpectedSamples[i]) {
			t.Fatalf("packet %d payload mismatch", i)
		}
	}
}

func TestDuplicatesCountedOnce(t *testing.T) {
	o := fixtures.DefaultRTPOptions()
	o.Duplicate = 3
	st, res := build(t, o)
	r := res[0]
	if r.Duplicate != len(st.DuplicateSequences) {
		t.Fatalf("duplicates = %d, want %d", r.Duplicate, len(st.DuplicateSequences))
	}
	var samples int
	for _, p := range r.Packets {
		if !p.Duplicate {
			samples++
		}
	}
	if samples != len(st.ExpectedSamples) {
		t.Fatalf("samples = %d, want %d", samples, len(st.ExpectedSamples))
	}
	// Duplicates must never be counted twice in output payloads.
	seen := map[string]bool{}
	for _, p := range r.Packets {
		if p.Duplicate {
			continue
		}
		key := string(p.Payload)
		if seen[key] {
			t.Fatal("duplicate payload emitted twice")
		}
		seen[key] = true
	}
}

func TestReorderRestored(t *testing.T) {
	o := fixtures.DefaultRTPOptions()
	o.Reorder = 5
	st, res := build(t, o)
	r := res[0]
	if r.OutOfOrder == 0 {
		t.Fatal("expected out-of-order packets to be observed")
	}
	// After recovery payloads are in sequence order.
	for i, p := range r.Packets {
		if string(p.Payload) != string(st.ExpectedSamples[i]) {
			t.Fatalf("packet %d out of order after recovery", i)
		}
	}
	if len(r.Gaps) != 0 {
		t.Fatalf("reorder must not create gaps: %v", r.Gaps)
	}
}

func TestLossBurstCreatesExplicitGap(t *testing.T) {
	o := fixtures.DefaultRTPOptions()
	o.LossBurst = 7
	o.LossAt = 20
	st, res := build(t, o)
	r := res[0]
	if len(r.Gaps) != 1 {
		t.Fatalf("gaps = %d, want 1", len(r.Gaps))
	}
	g := r.Gaps[0]
	if g.Missing != 7 {
		t.Fatalf("gap missing = %d, want 7", g.Missing)
	}
	if g.FromSeq != st.LostSequences[0] || g.ToSeq != st.LostSequences[len(st.LostSequences)-1] {
		t.Fatalf("gap range %d..%d, want %d..%d", g.FromSeq, g.ToSeq, st.LostSequences[0], st.LostSequences[len(st.LostSequences)-1])
	}
	// Samples are the received ones only; nothing is fabricated.
	var samples int
	for _, p := range r.Packets {
		if !p.Duplicate {
			samples++
		}
	}
	if samples != len(st.ExpectedSamples) {
		t.Fatalf("samples = %d, want %d", samples, len(st.ExpectedSamples))
	}
}

func TestSequenceWrap(t *testing.T) {
	o := fixtures.DefaultRTPOptions()
	st, err := fixtures.BuildRTPWrap(o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Recover(st.Packets)
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if len(r.Packets) != len(st.ExpectedSamples) {
		t.Fatalf("packets = %d", len(r.Packets))
	}
	if len(r.Gaps) != 0 {
		t.Fatalf("wrap must not create gaps: %v", r.Gaps)
	}
	// Recovery must cross the wrap point without ordering errors.
	var wrapped bool
	for i := 1; i < len(r.Packets); i++ {
		if r.Packets[i].Seq < r.Packets[i-1].Seq {
			wrapped = true
		}
		if string(r.Packets[i].Payload) != string(st.ExpectedSamples[i]) {
			t.Fatalf("packet %d payload mismatch across wrap", i)
		}
	}
	if !wrapped {
		t.Fatal("fixture did not cross the wrap point")
	}
	if r.OutOfOrder != 0 {
		t.Fatalf("wrap produced %d false out-of-order counts", r.OutOfOrder)
	}
}

func TestCombinedImpairments(t *testing.T) {
	o := fixtures.DefaultRTPOptions()
	o.Duplicate = 3
	o.Reorder = 5
	o.LossBurst = 7
	st, res := build(t, o)
	r := res[0]
	if r.Duplicate != len(st.DuplicateSequences) {
		t.Fatalf("duplicates = %d", r.Duplicate)
	}
	if len(r.Gaps) != 1 || r.Gaps[0].Missing != 7 {
		t.Fatalf("gaps = %+v", r.Gaps)
	}
	var samples int
	for _, p := range r.Packets {
		if !p.Duplicate {
			samples++
		}
	}
	if samples != len(st.ExpectedSamples) {
		t.Fatalf("samples = %d, want %d", samples, len(st.ExpectedSamples))
	}
}

func TestBlocksSplitFixedSize(t *testing.T) {
	o := fixtures.DefaultRTPOptions()
	o.LossBurst = 0
	_, res := build(t, o)
	blocks, gaps := Blocks(res[0], 16)
	if len(gaps) != 0 {
		t.Fatalf("unexpected gaps")
	}
	total := 0
	for i, b := range blocks {
		total += len(b)
		if i < len(blocks)-1 && len(b) != 16 {
			t.Fatalf("block %d has %d samples", i, len(b))
		}
	}
	if total != len(res[0].Packets) {
		t.Fatalf("blocks cover %d of %d packets", total, len(res[0].Packets))
	}
}

func TestDecodeStreamRoundTrip(t *testing.T) {
	st, err := fixtures.BuildRTP(fixtures.DefaultRTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	raw := fixtures.EncodeRTPStream(st.Packets)
	pkts, err := DecodeStream(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != len(st.Packets) {
		t.Fatalf("decoded %d of %d", len(pkts), len(st.Packets))
	}
	if _, err := DecodeStream(raw[:len(raw)-10]); err == nil {
		t.Fatal("expected truncation error")
	}
	if model.CodeOf(mustErr(DecodeStream(raw[:3]))) != model.ErrParamInvalid {
		t.Fatal("short header should be a param error")
	}
}

func mustErr(_ [][]byte, err error) error { return err }

func TestRecoverRejectsGarbage(t *testing.T) {
	_, err := Recover([][]byte{{0x01, 0x02}})
	if err == nil {
		t.Fatal("expected parse error")
	}
}
