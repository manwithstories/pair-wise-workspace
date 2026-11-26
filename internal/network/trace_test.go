package network

import "testing"

func TestGenerateDeterministicBySeed(t *testing.T) {
	a, err := Generate(ProfileBurst, 41)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(ProfileBurst, 41)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.JitterMS) != len(b.JitterMS) {
		t.Fatal("length changed")
	}
	for i := range a.JitterMS {
		if a.JitterMS[i] != b.JitterMS[i] {
			t.Fatalf("jitter[%d] differs: %d vs %d", i, a.JitterMS[i], b.JitterMS[i])
		}
	}
	if a.StartSequence != 65520 || !a.Has(ImpWrap, 16) {
		t.Fatal("sequence wrap fixture missing")
	}
}

func TestGenerateRejectsUnknownProfile(t *testing.T) {
	if _, err := Generate("nope", 1); err == nil {
		t.Fatal("expected unknown profile error")
	}
}

func TestBurstTraceContainsRequiredAnomalies(t *testing.T) {
	tr, err := Generate(ProfileBurst, 41)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{ImpBurstLoss, ImpRetransmit, ImpBandwidthDrop, ImpDuplicate, ImpReorder} {
		found := false
		for _, im := range tr.Impairments {
			if im.Kind == kind {
				found = true
			}
		}
		if !found {
			t.Fatalf("burst trace missing %s", kind)
		}
	}
	var lost, repairs int
	for _, im := range tr.Impairments {
		if im.Kind == ImpBurstLoss {
			lost += im.Length
		}
		if im.Kind == ImpRetransmit {
			repairs++
		}
	}
	if lost != 7 || repairs != 5 {
		t.Fatalf("lost=%d repairs=%d", lost, repairs)
	}
}

func TestCorruptTraceContainsMalformedAndConflict(t *testing.T) {
	tr, _ := Generate(ProfileCorrupt, 41)
	if !tr.Has(ImpTruncated, 6) || !tr.Has(ImpConflict, 11) {
		t.Fatalf("corrupt trace missing anomalies: %+v", tr.Impairments)
	}
}

func TestWaitWindowGrowsWhenBandwidthDrops(t *testing.T) {
	high := WaitWindowMS(Condition{BandwidthKbps: 1800, RTTMS: 24, JitterMS: 8})
	low := WaitWindowMS(Condition{BandwidthKbps: 320, RTTMS: 72, JitterMS: 26})
	if low <= high {
		t.Fatalf("window did not grow: high=%d low=%d", high, low)
	}
	if low > 115 {
		t.Fatalf("window must stay bounded for latency target: %d", low)
	}
}
