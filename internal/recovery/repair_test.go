package recovery

import (
	"testing"

	"mediaqc/internal/jitter"
	rtpio "mediaqc/internal/rtp"
)

func rpkt(seq uint64, payload string) *rtpio.Packet {
	return &rtpio.Packet{SSRC: 3, ExtSequence: seq, ExtTimestamp: seq * 1800, Payload: []byte(payload)}
}

func TestRepairWithRetransmissionCopy(t *testing.T) {
	r := NewRepairer(8)
	if err := r.AddRetransmission(rpkt(101, "repair")); err != nil {
		t.Fatal(err)
	}
	ordered := []*rtpio.Packet{rpkt(100, "a"), rpkt(102, "c")}
	gaps := []jitter.Gap{{Missing: []uint64{101}}}
	rep := r.Repair(ordered, gaps)

	if rep.Recovered != 1 || rep.RecoverableLoss != 1 || rep.RecoveryRate != 1 {
		t.Fatalf("report=%+v", rep)
	}
	if rep.UnrecoverableGap != 0 || len(rep.Frames) != 3 {
		t.Fatalf("frames/report wrong: %+v", rep)
	}
	f := rep.Frames[1]
	if !f.Repaired || f.Gap || string(f.Payload) != "repair" || f.Source != "retransmission" {
		t.Fatalf("repaired frame=%+v", f)
	}
}

func TestUnrecoverableLossIsExplicitGapAndExcludedFromRate(t *testing.T) {
	r := NewRepairer(8)
	ordered := []*rtpio.Packet{rpkt(200, "a")}
	gaps := []jitter.Gap{{Missing: []uint64{201, 202}}}
	rep := r.Repair(ordered, gaps)

	if rep.Recovered != 0 || rep.RecoverableLoss != 0 || rep.RecoveryRate != 1 {
		t.Fatalf("unrepairable gaps must not enter recovery denominator: %+v", rep)
	}
	if rep.UnrecoverableGap != 2 {
		t.Fatalf("gaps=%d", rep.UnrecoverableGap)
	}
	for _, f := range rep.Frames {
		if f.ExtSeq >= 201 {
			if !f.Gap || f.Payload != nil || f.Source != "gap" {
				t.Fatalf("gap must carry no fake audio: %+v", f)
			}
		}
	}
}

func TestMixedRecoverableAndUnrecoverable(t *testing.T) {
	r := NewRepairer(8)
	_ = r.AddRetransmission(rpkt(301, "copy"))
	gaps := []jitter.Gap{{Missing: []uint64{301, 302}}}
	rep := r.Repair(nil, gaps)
	if rep.Recovered != 1 || rep.RecoverableLoss != 1 || rep.UnrecoverableGap != 1 {
		t.Fatalf("report=%+v", rep)
	}
	if rep.RecoveryRate < 0.999 {
		t.Fatalf("rate=%.3f want 1/1 recoverable", rep.RecoveryRate)
	}
}

func TestRetransmissionCacheBounded(t *testing.T) {
	r := NewRepairer(1)
	if err := r.AddRetransmission(rpkt(1, "a")); err != nil {
		t.Fatal(err)
	}
	if err := r.AddRetransmission(rpkt(2, "b")); err == nil {
		t.Fatal("expected bounded cache error")
	}
}
