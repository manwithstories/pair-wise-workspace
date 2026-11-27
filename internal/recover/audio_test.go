package recover

import (
	"math"
	"testing"

	"mediacheck/internal/model"
	"mediacheck/internal/opus"
)

func realPackets(t *testing.T, n int, fec bool) [][]byte {
	t.Helper()
	enc, err := opus.NewEncoder(48000, 2, 32000, opus.ApplicationVoIP, fec, 25)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var out [][]byte
	for i := 0; i < n; i++ {
		pcm := make([]int16, 960*2)
		for j := 0; j < 960; j++ {
			v := int16(7000 * math.Sin(2*math.Pi*440*float64(i*960+j)/48000))
			pcm[2*j], pcm[2*j+1] = v, v
		}
		data, err := enc.Encode(pcm, 960)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, data)
	}
	return out
}

func slotAt(i int) model.Slot {
	const ms20 = int64(20_000_000)
	return model.Slot{Start: int64(i) * ms20, End: int64(i+1) * ms20}
}

func TestFillerCapabilitiesAndClose(t *testing.T) {
	f, caps, err := NewAudioFiller()
	if err != nil {
		t.Fatal(err)
	}
	if !caps.FEC || !caps.PLC || !caps.Silence {
		t.Fatalf("capabilities = %+v, want all true with libopus", caps)
	}
	f.Close()
	f.Close()
	if err := f.Observe([]byte{0x78, 0x01}, 960); err == nil {
		t.Fatal("Observe after Close should fail")
	}
}

func TestFillUsesFECWhenRedundancyExists(t *testing.T) {
	pkts := realPackets(t, 6, true)
	f, _, err := NewAudioFiller()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Feed packets 0..2, then lose 3 and let packet 4 look ahead.
	for i := 0; i < 3; i++ {
		if err := f.Observe(pkts[i], 960); err != nil {
			t.Fatal(err)
		}
	}
	res := f.Fill(slotAt(3), 960, pkts[4])
	if res.Method != MethodFEC {
		t.Fatalf("method = %v, want fec", res.Method)
	}
	if len(res.Data) == 0 {
		t.Fatal("FEC produced no packet")
	}
	if res.Reason != "in-band FEC" {
		t.Fatalf("reason = %q", res.Reason)
	}
}

func TestFillUsesPLCWithoutLookahead(t *testing.T) {
	pkts := realPackets(t, 5, false)
	f, _, err := NewAudioFiller()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < 3; i++ {
		if err := f.Observe(pkts[i], 960); err != nil {
			t.Fatal(err)
		}
	}
	res := f.Fill(slotAt(3), 960, nil)
	if res.Method != MethodPLC {
		t.Fatalf("method = %v, want plc", res.Method)
	}
	if len(res.Data) == 0 {
		t.Fatal("PLC produced no packet")
	}
}

func TestFillFallsBackToSilenceWhenDecoderClosed(t *testing.T) {
	f, _, err := NewAudioFiller()
	if err != nil {
		t.Fatal(err)
	}
	// Close only the decoder to simulate a stream where FEC and PLC are not
	// possible but the encoder still works.
	f.dec.Close()
	f.dec = nil
	defer f.Close()
	res := f.Fill(slotAt(2), 960, nil)
	if res.Method != MethodSilence {
		t.Fatalf("method = %v, want silence", res.Method)
	}
	if len(res.Data) == 0 {
		t.Fatal("silence produced no packet")
	}
	if err := f.Observe([]byte{0x78}, 960); err == nil {
		t.Fatal("Observe without decoder should fail")
	}
}

func TestFECAndPLCNeverClaimOriginalWaveform(t *testing.T) {
	pkts := realPackets(t, 5, true)
	f, _, err := NewAudioFiller()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < 3; i++ {
		if err := f.Observe(pkts[i], 960); err != nil {
			t.Fatal(err)
		}
	}
	res := f.Fill(slotAt(3), 960, pkts[4])
	for _, m := range []Method{MethodFEC, MethodPLC, MethodSilence} {
		if m.String() == "" || m.String() == "restored" {
			t.Fatalf("method name %q implies restoration", m.String())
		}
	}
	if res.Method.FaultKind() != model.FaultFECRecovered {
		t.Fatalf("FEC fault kind = %v", res.Method.FaultKind())
	}
}

func TestTrackerCountsOnlyFilledSlots(t *testing.T) {
	tr := NewTracker()
	good := []byte{0x78, 0x01}
	tr.Record(SlotResult{Slot: slotAt(0), Method: MethodFEC, Data: good, Reason: "in-band FEC"})
	tr.Record(SlotResult{Slot: slotAt(1), Method: MethodPLC, Data: good, Reason: "packet loss concealment"})
	tr.Record(SlotResult{Slot: slotAt(2), Method: MethodSilence, Data: good, Reason: "silence placeholder"})
	tr.Record(SlotResult{Slot: slotAt(3), Method: MethodGiveUp, Reason: "all methods failed"})
	o := tr.Outcomes()
	if o.Slots != 4 || o.FEC != 1 || o.PLC != 1 || o.Silence != 1 || o.Failed != 1 {
		t.Fatalf("outcomes = %+v", o)
	}
	if got := o.RecoveryRate(); got != 75 {
		t.Fatalf("recovery rate = %v, want 75", got)
	}
	// The four slots are contiguous, so they merge into one damaged range.
	if len(o.Damage()) != 1 {
		t.Fatalf("damaged ranges = %d, want 1 merged range", len(o.Damage()))
	}
	if d := o.Damage()[0]; d.Start != 0 || d.End != slotAt(3).End {
		t.Fatalf("merged range = %+v", d)
	}
	if len(o.Faults()) != 4 {
		t.Fatalf("faults = %d, want 4", len(o.Faults()))
	}
}

func TestTrackerKeepsDisjointRangesSeparate(t *testing.T) {
	tr := NewTracker()
	tr.Record(SlotResult{Slot: model.Slot{Start: 0, End: 1_000_000}, Method: MethodFEC, Data: []byte{1}})
	tr.Record(SlotResult{Slot: model.Slot{Start: 5_000_000, End: 6_000_000}, Method: MethodPLC, Data: []byte{1}})
	if got := len(tr.Outcomes().Damage()); got != 2 {
		t.Fatalf("damaged ranges = %d, want 2", got)
	}
}

func TestTrackerFullRecoveryRate(t *testing.T) {
	tr := NewTracker()
	tr.Record(SlotResult{Slot: slotAt(0), Method: MethodFEC, Data: []byte{1}})
	tr.Record(SlotResult{Slot: slotAt(1), Method: MethodFEC, Data: []byte{1}})
	if got := tr.Outcomes().RecoveryRate(); got != 100 {
		t.Fatalf("recovery rate = %v, want 100", got)
	}
	empty := NewTracker().Outcomes()
	if got := empty.RecoveryRate(); got != 100 {
		t.Fatalf("empty recovery rate = %v, want 100 (no slots lost)", got)
	}
}

func TestOutcomesMerge(t *testing.T) {
	a := NewTracker()
	a.Record(SlotResult{Slot: slotAt(0), Method: MethodFEC, Data: []byte{1}})
	b := NewTracker()
	b.Record(SlotResult{Slot: slotAt(1), Method: MethodGiveUp})
	o := a.Outcomes()
	o.Merge(b.Outcomes())
	if o.Slots != 2 || o.FEC != 1 || o.Failed != 1 {
		t.Fatalf("merged outcomes = %+v", o)
	}
	if got := o.RecoveryRate(); got != 50 {
		t.Fatalf("merged recovery rate = %v, want 50", got)
	}
}

func TestCloseDuringRecoveryReleasesLibopus(t *testing.T) {
	pkts := realPackets(t, 4, true)
	f, _, err := NewAudioFiller()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Observe(pkts[0], 960); err != nil {
		t.Fatal(err)
	}
	// A cancel mid-stream closes the filler; subsequent use must report the
	// closed state instead of dereferencing freed libopus handles.
	f.Close()
	res := f.Fill(slotAt(1), 960, pkts[2])
	if res.Method != MethodGiveUp {
		t.Fatalf("Fill after Close = %v, want give up", res.Method)
	}
	if res.Data != nil {
		t.Fatal("Fill after Close returned data from freed handles")
	}
	if err := f.Observe(pkts[1], 960); err == nil {
		t.Fatal("Observe after Close succeeded")
	}
}
