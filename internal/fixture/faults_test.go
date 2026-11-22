package fixture_test

import (
	"testing"

	"mediapack/internal/fixture"
	"mediapack/internal/index"
	"mediapack/internal/rtp"
)

func TestBadIndexVariants(t *testing.T) {
	const size = int64(4096)
	cases := map[fixture.BadIndexKind]bool{
		fixture.BadIndexMagic:        true,
		fixture.BadIndexVersion:      true,
		fixture.BadIndexOutOfRange:   true,
		fixture.BadIndexPartialRecord: true,
	}
	for kind := range cases {
		blob := fixture.BuildBadIndex(kind, size)
		if _, _, err := index.Parse(blob, size); err == nil {
			t.Fatalf("bad index kind %d must be rejected", kind)
		}
	}
}

func TestDuplicateAndWrappingRTP(t *testing.T) {
	pkts, err := fixture.BuildDuplicateRTP(fixture.RTPSpec{
		SSRC: 1, StartSeq: 10, Count: 20,
	}, 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	v := rtp.NewValidator(1)
	var dups, gaps int
	for _, raw := range pkts {
		_, ev, err := v.Check(raw)
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Issue {
		case rtp.IssueDuplicate:
			dups++
		case rtp.IssueGap:
			gaps++
		}
	}
	if dups != 1 || gaps != 1 {
		t.Fatalf("dups=%d gaps=%d", dups, gaps)
	}

	wrap, err := fixture.BuildWrappingRTP(99, 40)
	if err != nil {
		t.Fatal(err)
	}
	v2 := rtp.NewValidator(1)
	for _, raw := range wrap {
		if _, ev, err := v2.Check(raw); err != nil {
			t.Fatal(err)
		} else {
			_ = ev
		}
	}
	if v2.Stats().SequenceWraps < 1 {
		t.Fatal("16-bit sequence wrap not observed")
	}
}

func TestWrappingTimestampFixture(t *testing.T) {
	ts := fixture.WrappingTimestampAU(8)
	crossed := false
	for i := 1; i < len(ts); i++ {
		if ts[i] < ts[i-1] {
			crossed = true
		}
	}
	if !crossed {
		t.Fatal("fixture must produce a 32-bit timestamp wraparound")
	}
}
