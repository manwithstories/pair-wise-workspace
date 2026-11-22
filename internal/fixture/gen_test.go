package fixture

import (
	"testing"

	"mediatool/internal/clock"
	"mediatool/internal/ts"
)

func parseAll(t *testing.T, s Stream) ts.ParseResult {
	t.Helper()
	res := ts.Parse(s.Data)
	if len(res.Packets) == 0 {
		t.Fatal("no packets parsed")
	}
	return res
}

func hasFault(fs clock.Faults, k clock.FaultKind) bool {
	for _, f := range fs {
		if f.Kind == k {
			return true
		}
	}
	return false
}

// 四类夹具：成功、损坏、回绕、跨 PSI 版本边界。
func TestFixtureClean(t *testing.T) {
	s := Generate(42, 4000, FaultSpec{})
	res := parseAll(t, s)
	if len(res.Faults) != 0 {
		t.Fatalf("clean fixture has faults: %v", res.Faults)
	}
	// 往返一致：每个包都能逐字节重建。
	for i, p := range res.Packets {
		if string(p.Marshal()) != string(s.Data[i*ts.PacketSize:(i+1)*ts.PacketSize]) {
			t.Fatalf("packet %d roundtrip mismatch", i)
		}
	}
}

func TestFixtureCorrupt(t *testing.T) {
	s := Generate(42, 4000, FaultSpec{CCConflict: true})
	res := parseAll(t, s)
	if !hasFault(res.Faults, clock.FaultCCConflict) {
		t.Fatalf("want cc-conflict, got %v", res.Faults)
	}
}

func TestFixtureWrap(t *testing.T) {
	s := Generate(42, 8000, FaultSpec{PCRWrap: true})
	res := parseAll(t, s)
	var u clock.Unwrapper
	wrapped := false
	for _, p := range res.Packets {
		if !p.HasPCR {
			continue
		}
		v := u.Unwrap(uint64(p.PCRBase))
		if v >= clock.Mod33 {
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatal("PCR never crossed the wrap boundary")
	}
}

func TestFixturePSIVersionBoundary(t *testing.T) {
	s := Generate(42, 4000, FaultSpec{PSIVersion: true})
	res := parseAll(t, s)
	a := ts.NewPSIAssembler()
	var faults clock.Faults
	for _, p := range res.Packets {
		_, f := a.Feed(p)
		faults = append(faults, f...)
	}
	faults = append(faults, a.Flush()...)
	if !hasFault(faults, clock.FaultPSIVersion) {
		t.Fatalf("want psi-version fault, got %v", faults)
	}
	pmt, ok := a.LastValidPMT(PIDPMT)
	if !ok || pmt.Version != 1 {
		t.Fatalf("last valid PMT = %+v ok=%v", pmt, ok)
	}
}

func TestFixtureTruncatedPSI(t *testing.T) {
	s := Generate(42, 4000, FaultSpec{TruncatedPSI: true})
	res := parseAll(t, s)
	a := ts.NewPSIAssembler()
	for _, p := range res.Packets {
		a.Feed(p)
	}
	faults := a.Flush()
	if !hasFault(faults, clock.FaultPSIIncomplete) {
		t.Fatalf("want truncated-psi, got %v", faults)
	}
	if _, ok := a.LastValidPMT(PIDPMT); !ok {
		t.Fatal("last valid PMT must survive truncation")
	}
}

func TestFixtureDeterministic(t *testing.T) {
	a := Generate(42, 4000, FaultSpec{PCRWrap: true, CCGap: true})
	b := Generate(42, 4000, FaultSpec{PCRWrap: true, CCGap: true})
	if string(a.Data) != string(b.Data) {
		t.Fatal("same seed produced different streams")
	}
	if len(a.RTP) != len(b.RTP) {
		t.Fatal("RTP feed not deterministic")
	}
}

func TestParseFaultList(t *testing.T) {
	f, err := ParseFaultList("pcr-wrap,cc-gap,psi-version,audio-rate,color-conflict")
	if err != nil {
		t.Fatal(err)
	}
	if !(f.PCRWrap && f.CCGap && f.PSIVersion && f.AudioRate && f.ColorConflict) {
		t.Fatalf("spec = %+v", f)
	}
	if _, err := ParseFaultList("bogus"); err == nil {
		t.Fatal("want error for unknown fault")
	}
}
