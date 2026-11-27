package merge

import (
	"context"
	"testing"

	"avpipe/internal/media"
)

func pkt(track string, seq, pts, dts int64, base media.Rational, data string) media.Packet {
	return media.Packet{
		TrackID: track, Seq: seq, Arrival: seq, PTS: pts, DTS: dts,
		TimeBase: base, Data: []byte(data),
		Origin: media.Origin{File: "f", TrackID: track, PacketSeq: seq},
	}
}

func TestMergeOrdersByPTSThenTrackThenSeq(t *testing.T) {
	base := media.MustRat(1, 30)
	m := New(Policy{})
	// Deliberately interleave tracks; PTS ties are broken by track ID then seq.
	// DTS is kept monotonic per track, as a well-formed source would be.
	adds := []media.Packet{
		pkt("a0", 0, 0, 0, base, "a0"),
		pkt("v0", 0, 1, 1, base, "v1"),
		pkt("v0", 1, 0, 2, base, "v0"), // PTS 0 but decodes later; still legal
		pkt("a0", 1, 2, 2, base, "a2"),
	}
	for _, p := range adds {
		if _, err := m.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	out, err := m.Sorted()
	if err != nil {
		t.Fatal(err)
	}
	// Expected: pts 0 (a0#0, v0#0), pts 1 (v0#0), pts 2 (a0#1)
	got := []string{}
	for _, p := range out {
		got = append(got, string(p.Data))
	}
	want := []string{"a0", "v0", "v1", "a2"}
	if len(got) != len(want) {
		t.Fatalf("len %d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pos %d = %s want %s (%v)", i, got[i], want[i], got)
		}
	}
}

func TestMergeDropsDuplicates(t *testing.T) {
	base := media.MustRat(1, 30)
	m := New(Policy{})
	p := pkt("v0", 0, 0, 0, base, "x")
	m.Add(p)          //nolint:errcheck
	m.Add(p)          //nolint:errcheck
	m.Add(p.Clone())  //nolint:errcheck
	out, _ := m.Sorted()
	if len(out) != 1 {
		t.Fatalf("duplicates emitted: %d", len(out))
	}
	if st := m.Stats(); st.Duplicates != 2 {
		t.Fatalf("duplicates = %d", st.Duplicates)
	}
}

func TestMergePreservesVFR(t *testing.T) {
	base := media.MustRat(1, 90000)
	m := New(Policy{})
	// Uneven spacing: 3000, 3000, 3000, 9000, 3000 ticks.
	pts := []int64{0, 3000, 6000, 9000, 18000, 21000}
	for i, p := range pts {
		if _, err := m.Add(pkt("v0", int64(i), p, p, base, string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	out, err := m.Sorted()
	if err != nil {
		t.Fatal(err)
	}
	for i := range out {
		if out[i].PTS != pts[i] {
			t.Fatalf("PTS %d moved to %d", pts[i], out[i].PTS)
		}
	}
}

func TestMergeQuarantinesDTSRegression(t *testing.T) {
	base := media.MustRat(1, 30)
	m := New(Policy{AllowDTSRegression: true})
	if _, err := m.Add(pkt("v0", 0, 0, 10, base, "a")); err != nil {
		t.Fatal(err)
	}
	ok, err := m.Add(pkt("v0", 1, 1, 5, base, "b")) // DTS regressed
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("regressed packet was emitted")
	}
	if st := m.Stats(); st.Reasons[ReasonDTSRegression] != 1 {
		t.Fatalf("reasons = %v", st.Reasons)
	}
}

func TestMergeFatalDTSRegression(t *testing.T) {
	base := media.MustRat(1, 30)
	m := New(Policy{})
	m.Add(pkt("v0", 0, 0, 10, base, "a")) //nolint:errcheck
	_, err := m.Add(pkt("v0", 1, 1, 5, base, "b"))
	if err == nil {
		t.Fatal("expected fatal regression")
	}
	if media.KindOf(err) != media.KindPolicy {
		t.Fatalf("kind = %v", media.KindOf(err))
	}
}

func TestMergeMissingPTSQuarantined(t *testing.T) {
	base := media.MustRat(1, 30)
	m := New(Policy{AllowMissingTimestamp: true})
	ok, err := m.Add(pkt("v0", 0, media.NoTimestamp, 0, base, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("missing-PTS packet emitted")
	}
	if st := m.Stats(); st.Reasons[ReasonMissingTimestamp] != 1 {
		t.Fatalf("reasons = %v", st.Reasons)
	}
}

func TestMergeMissingPTSFatal(t *testing.T) {
	base := media.MustRat(1, 30)
	m := New(Policy{})
	_, err := m.Add(pkt("v0", 0, media.NoTimestamp, 0, base, "a"))
	if media.KindOf(err) != media.KindCorrupt {
		t.Fatalf("kind = %v", media.KindOf(err))
	}
}

func TestValidateOrderCatchesRegression(t *testing.T) {
	base := media.MustRat(1, 30)
	pkts := []media.Packet{
		pkt("v0", 0, 30, 30, base, "a"),
		pkt("v0", 1, 10, 10, base, "b"),
	}
	if err := ValidateOrder(pkts); err == nil {
		t.Fatal("expected regression error")
	}
}

func TestValidateOrderCatchesDuplicates(t *testing.T) {
	base := media.MustRat(1, 30)
	p := pkt("v0", 0, 0, 0, base, "a")
	if err := ValidateOrder([]media.Packet{p, p}); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestMergeRunContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := New(Policy{})
	ch := make(chan media.Packet)
	if _, err := m.Run(ctx, ch); err == nil || media.KindOf(err) != media.KindCancel {
		t.Fatalf("err = %v", err)
	}
}

func TestGroupByTrack(t *testing.T) {
	base := media.MustRat(1, 30)
	pkts := []media.Packet{
		pkt("v0", 0, 0, 0, base, "a"),
		pkt("a1", 0, 0, 0, base, "b"),
		pkt("v0", 1, 1, 1, base, "c"),
	}
	g := GroupByTrack(pkts)
	if len(g["v0"]) != 2 || len(g["a1"]) != 1 {
		t.Fatalf("grouping wrong: %+v", g)
	}
	if string(g["v0"][1].Data) != "c" {
		t.Fatal("relative order lost")
	}
}
