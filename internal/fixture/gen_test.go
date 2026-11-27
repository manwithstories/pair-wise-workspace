package fixture

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"archiver/internal/media"
	"archiver/internal/rtp"
)

func parseAll(t *testing.T, stream []byte) (pkts []*rtp.Packet, errs []*media.PacketError) {
	t.Helper()
	rd := rtp.NewReader(bytes.NewReader(stream), 0)
	for {
		p, err := rd.Next()
		if err == io.EOF {
			return pkts, errs
		}
		var pe *media.PacketError
		if errors.As(err, &pe) {
			errs = append(errs, pe)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, p)
	}
}

func TestCleanStreamParses(t *testing.T) {
	pkts, errs := parseAll(t, Generate(Options{Seed: 73}))
	if len(errs) != 0 {
		t.Fatalf("clean stream errors: %v", errs[0])
	}
	if len(pkts) < 900 {
		t.Fatalf("packets = %d", len(pkts))
	}
	var sr int
	for _, p := range pkts {
		if p.RTCP {
			sr++
		}
	}
	if sr == 0 {
		t.Fatal("clean stream must carry sender reports")
	}
}

func TestDeterministic(t *testing.T) {
	a := Generate(Options{Seed: 73, Inject: []string{"duplicate", "wrap"}})
	b := Generate(Options{Seed: 73, Inject: []string{"duplicate", "wrap"}})
	if !bytes.Equal(a, b) {
		t.Fatal("same seed must produce identical streams")
	}
	c := Generate(Options{Seed: 74})
	if bytes.Equal(a, c) {
		t.Fatal("different seeds must differ")
	}
}

func TestShortHeaderFixture(t *testing.T) {
	_, errs := parseAll(t, Generate(Options{Seed: 1, Inject: []string{"short-header"}}))
	if len(errs) != 1 {
		t.Fatalf("errors = %d, want 1", len(errs))
	}
}

func TestTruncatedExtensionFixture(t *testing.T) {
	_, errs := parseAll(t, Generate(Options{Seed: 1, Inject: []string{"truncated-extension"}}))
	if len(errs) != 1 {
		t.Fatalf("errors = %d, want 1", len(errs))
	}
	if errs[0].Offset <= 0 {
		t.Fatalf("first anomalous offset = %d", errs[0].Offset)
	}
}

func TestWrapDuplicateFixture(t *testing.T) {
	stream := Generate(Options{Seed: 2, Inject: []string{"wrap", "duplicate"}})
	pkts, errs := parseAll(t, stream)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs[0])
	}
	r := rtp.NewReorderer(128)
	var n int
	for _, p := range pkts {
		if p.RTCP {
			continue
		}
		em, perr := r.Push(p)
		if perr != nil {
			t.Fatalf("out of window: %v", perr)
		}
		n += len(em)
	}
	n += len(r.Flush())
	st := r.Stats()
	if st.Duplicates == 0 {
		t.Fatal("duplicate injection must yield duplicates")
	}
	// Wrap: audio starts at 65500, so 600 packets wrap the 16-bit space.
	if st.Lost != 0 {
		t.Fatalf("wrap must not lose packets: %+v", st)
	}
}

func TestDriftNoRTCPFixture(t *testing.T) {
	pkts, errs := parseAll(t, Generate(Options{Seed: 3, Inject: []string{"drift-no-rtcp"}}))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs[0])
	}
	for _, p := range pkts {
		if p.RTCP {
			t.Fatal("drift-no-rtcp must omit sender reports")
		}
	}
}

func TestLateVFRFixture(t *testing.T) {
	stream := Generate(Options{Seed: 4, Inject: []string{"late-vfr"}})
	pkts, errs := parseAll(t, stream)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs[0])
	}
	// The held-back packets arrive after later sequence numbers.
	r := rtp.NewReorderer(128)
	late := 0
	for _, p := range pkts {
		if p.RTCP || p.SSRC != VideoSSRC {
			continue
		}
		if _, perr := r.Push(p); perr != nil {
			late++
		}
	}
	if late == 0 {
		t.Fatal("late-vfr must push at least one packet out of the window")
	}
}

func TestBadFrameFixture(t *testing.T) {
	// bad-frame keeps RTP parsing clean but breaks one frame CRC; the
	// video package tests verify index behavior on such streams.
	stream := Generate(Options{Seed: 5, Inject: []string{"bad-frame"}})
	_, errs := parseAll(t, stream)
	if len(errs) != 0 {
		t.Fatalf("bad-frame must not affect RTP parsing: %v", errs[0])
	}
}
