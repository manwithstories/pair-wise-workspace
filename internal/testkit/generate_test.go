package testkit

import "testing"

func TestGenerateDeterministic(t *testing.T) {
	a, err := Generate(Options{Seed: 7, DurationNs: 10e9, Bitrates: []int{800, 1600}, Loss: 0.05, Case: CaseSuccess})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(Options{Seed: 7, DurationNs: 10e9, Bitrates: []int{800, 1600}, Loss: 0.05, Case: CaseSuccess})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Renditions) != len(b.Renditions) {
		t.Fatalf("rendition count differs")
	}
	for i := range a.Renditions {
		if string(a.Renditions[i].Master) != string(b.Renditions[i].Master) {
			t.Fatalf("rendition %d master differs between runs", i)
		}
		if string(a.Renditions[i].Index) != string(b.Renditions[i].Index) {
			t.Fatalf("rendition %d index differs between runs", i)
		}
	}
	if len(a.RTP.Wire) != len(b.RTP.Wire) {
		t.Fatalf("RTP wire length differs")
	}
}

func TestGenerateRejectsBadOptions(t *testing.T) {
	cases := []Options{
		{Seed: 1, DurationNs: 0, Bitrates: []int{800}},
		{Seed: 1, DurationNs: 10e9, Bitrates: nil},
		{Seed: 1, DurationNs: 10e9, Bitrates: []int{800}, Case: "nope"},
		{Seed: 1, DurationNs: 10e9, Bitrates: []int{800}, Loss: 1.5},
	}
	for _, c := range cases {
		if _, err := Generate(c); err == nil {
			t.Errorf("accepted invalid options %+v", c)
		}
	}
}

func TestRTPRecoveryRateAndCounts(t *testing.T) {
	f, err := GenerateRTP(11, 0.2, 2000)
	if err != nil {
		t.Fatal(err)
	}
	st := RecoverRTP(f)
	if st.Received == 0 || st.UniquePackets == 0 {
		t.Fatalf("empty recovery: %+v", st)
	}
	if st.RecoveryRate < 0.7 || st.RecoveryRate > 1 {
		t.Fatalf("recovery rate out of range for 20%% loss: %+v", st)
	}
	if st.Duplicates == 0 {
		t.Fatalf("fixture produced no duplicates: %+v", st)
	}
	if st.Reordered == 0 {
		t.Fatalf("fixture produced no reordering: %+v", st)
	}
	// Unique packets can never exceed what was received or what was sent.
	if st.UniquePackets > st.Received || uint64(st.UniquePackets) > f.Sent {
		t.Fatalf("impossible counts: %+v sent=%d", st, f.Sent)
	}
}

func TestRTPZeroLoss(t *testing.T) {
	f, err := GenerateRTP(3, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	st := RecoverRTP(f)
	if st.RecoveryRate < 0.99 {
		t.Fatalf("zero-loss recovery=%f", st.RecoveryRate)
	}
}

func TestRTPLatencyBounded(t *testing.T) {
	f, err := GenerateRTP(5, 0.03, 1000)
	if err != nil {
		t.Fatal(err)
	}
	lat := f.LatencyMs()
	if len(lat) == 0 {
		t.Fatal("no latency samples")
	}
	p95 := P95(lat)
	if p95 > 500 {
		t.Fatalf("p95 latency %.1f ms exceeds bound", p95)
	}
	for _, v := range lat {
		if v < 0 {
			t.Fatalf("negative latency %f", v)
		}
	}
}

func TestP95NearestRank(t *testing.T) {
	if P95(nil) != 0 {
		t.Fatal("empty p95 must be zero")
	}
	vals := make([]float64, 100)
	for i := range vals {
		vals[i] = float64(i + 1)
	}
	if got := P95(vals); got != 95 {
		t.Fatalf("p95=%f want 95", got)
	}
	if got := P95([]float64{7}); got != 7 {
		t.Fatalf("single p95=%f", got)
	}
}

func TestInputsCoverEveryArtifact(t *testing.T) {
	fx, err := Generate(Options{Seed: 1, DurationNs: 10e9, Bitrates: []int{800, 1600}, Loss: 0.03, Case: CaseSuccess})
	if err != nil {
		t.Fatal(err)
	}
	in := fx.Inputs()
	if len(in) != 2*len(fx.Renditions) {
		t.Fatalf("inputs=%d renditions=%d", len(in), len(fx.Renditions))
	}
	for name, b := range in {
		if len(b) == 0 {
			t.Fatalf("artifact %q is empty", name)
		}
	}
}
