package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"tsqc/internal/ts"
)

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestGenerateIsSeededAndDeterministic(t *testing.T) {
	a, err := Generate(DefaultOptions(41))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(DefaultOptions(41))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Generate(DefaultOptions(42))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.TS, b.TS) || digest(a.TS) != digest(b.TS) {
		t.Fatal("same seed produced different streams")
	}
	if bytes.Equal(a.TS, c.TS) {
		t.Fatal("different seeds produced identical streams")
	}
	if len(a.Groups) == 0 {
		t.Fatal("no FEC groups")
	}
}

func TestGenerateParsesCleanly(t *testing.T) {
	st, err := Generate(DefaultOptions(41))
	if err != nil {
		t.Fatal(err)
	}
	pkts, trunc, err := ts.SplitPackets(st.TS)
	if err != nil || trunc != 0 {
		t.Fatalf("split err=%v trunc=%d", err, trunc)
	}
	d := ts.NewDemux()
	var aus []ts.AccessUnit
	for _, raw := range pkts {
		got, err := d.PushPacket(raw)
		if err != nil {
			t.Fatalf("clean stream parse error: %v", err)
		}
		aus = append(aus, got...)
	}
	aus = append(aus, d.Flush()...)
	var idrs int
	for _, au := range aus {
		if au.Keyframe {
			idrs++
		}
	}
	if idrs < 4 {
		t.Fatalf("idrs=%d", idrs)
	}
}

func TestFaultSetsValidate(t *testing.T) {
	if _, err := ParseFaults([]string{"nope"}); err == nil {
		t.Fatal("unknown fault accepted")
	}
	all, err := ParseFaults([]string{"all"})
	if err != nil || len(all) != len(AllFaults) {
		t.Fatalf("all=%v err=%v", all, err)
	}
	st, err := Generate(Options{Seed: 7, IDRs: 6, GOP: 3, AudioPerGOP: 2, Faults: all})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.TS) == 0 || len(st.Packets) == 0 {
		t.Fatal("empty fault stream")
	}
	// All faults together must stay deterministic.
	again, _ := Generate(Options{Seed: 7, IDRs: 6, GOP: 3, AudioPerGOP: 2, Faults: all})
	if digest(st.TS) != digest(again.TS) {
		t.Fatal("fault stream not deterministic")
	}
}
