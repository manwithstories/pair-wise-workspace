package rtp_test

import (
	"testing"

	"mediapack/internal/fixture"
	"mediapack/internal/rtp"
)

func TestCleanStream(t *testing.T) {
	pkts, err := fixture.BuildRTP(fixture.RTPSpec{
		SSRC: 0xDEADBEEF, PayloadTyp: 96, StartSeq: 100, BaseTS: 9000,
		TSInc: 3000, Count: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := rtp.NewValidator(1)
	for _, raw := range pkts {
		pkt, ev, err := v.Check(raw)
		if err != nil {
			t.Fatal(err)
		}
		if pkt.SSRC != 0xDEADBEEF || ev.Issue != rtp.IssueOK {
			t.Fatalf("unexpected event %+v", ev)
		}
	}
	st := v.Stats()
	if st.Packets != 50 || st.Duplicates != 0 || st.Gaps != 0 || st.Reorders != 0 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

func TestDuplicateGapReorderAndWrap(t *testing.T) {
	pkts, _ := fixture.BuildRTP(fixture.RTPSpec{
		SSRC: 1, StartSeq: 0xFFF8, BaseTS: 0, TSInc: 100, Count: 12,
	})
	// Inject: duplicate pkt0, drop pkt3, reorder pkt5 before pkt4.
	mut := make([][]byte, 0, len(pkts)+1)
	mut = append(mut, pkts[0], pkts[0]) // duplicate
	for i, p := range pkts[1:] {
		seq := i + 1
		if seq == 3 {
			continue // gap
		}
		if seq == 5 {
			mut = append(mut, pkts[5], pkts[4]) // reorder
			continue
		}
		if seq == 4 {
			continue // already consumed in the swap above
		}
		mut = append(mut, p)
	}
	v := rtp.NewValidator(2)
	var sawDup, sawGap, sawReorder, sawWrap bool
	for _, raw := range mut {
		_, ev, err := v.Check(raw)
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Issue {
		case rtp.IssueDuplicate:
			sawDup = true
		case rtp.IssueGap:
			sawGap = true // pkt3 lost; and the 4/5 swap lands before pkt6
		case rtp.IssueReorder:
			sawReorder = true
		case rtp.IssueWrap:
			sawWrap = true
		}
	}
	st := v.Stats()
	if !sawDup || st.Duplicates != 1 {
		t.Fatalf("duplicate not detected: %+v", st)
	}
	if !sawGap || st.Gaps != 2 {
		t.Fatalf("gap count wrong: %+v", st)
	}
	if !sawReorder || st.Reorders != 1 {
		t.Fatalf("reorder not detected: %+v", st)
	}
	if !sawWrap || st.SequenceWraps != 1 {
		t.Fatalf("16-bit wrap not detected: %+v", st)
	}
}

func TestSSRCChange(t *testing.T) {
	pkts, _ := fixture.BuildRTP(fixture.RTPSpec{
		SSRC: 7, StartSeq: 1, Count: 3,
	})
	pkts2, _ := fixture.BuildRTP(fixture.RTPSpec{
		SSRC: 8, StartSeq: 4, Count: 2,
	})
	v := rtp.NewValidator(1)
	for _, p := range pkts {
		if _, ev, err := v.Check(p); err != nil || ev.Issue != rtp.IssueOK {
			t.Fatalf("initial stream: %+v err=%v", ev, err)
		}
	}
	var changed bool
	for _, p := range pkts2 {
		_, ev, err := v.Check(p)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Issue == rtp.IssueSSRCChange {
			changed = true
		}
	}
	if !changed || v.Stats().SSRCChanges != 1 {
		t.Fatalf("ssrc change not detected")
	}
}

func TestMalformedPacket(t *testing.T) {
	v := rtp.NewValidator(1)
	if _, _, err := v.Check([]byte{0x10}); err == nil {
		t.Fatal("truncated RTP packet must be rejected")
	}
}
