package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"example.com/medialab/archive/internal/codec"
	"example.com/medialab/archive/internal/model"
)

func TestGenerateDeterministic(t *testing.T) {
	o := OpusStream(42, 4_000_000, 10_000_000, 2_000_000)
	a, _, err := Generate(o)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := Generate(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Records) != len(b.Records) || len(a.Records) == 0 {
		t.Fatalf("record counts %d/%d", len(a.Records), len(b.Records))
	}
	for i := range a.Records {
		if a.Records[i].ArrivalUS != b.Records[i].ArrivalUS || string(a.Records[i].Bytes) != string(b.Records[i].Bytes) {
			t.Fatalf("record %d differs between runs", i)
		}
	}
	// A different seed changes the payload mix.
	c, _, err := Generate(OpusStream(43, 4_000_000, 10_000_000, 2_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Records) == len(a.Records) && string(c.Records[0].Bytes) == string(a.Records[0].Bytes) && len(a.Packets) == len(c.Packets) {
		// Different seeds may still agree structurally; compare a digest of all bytes.
		ha, hc := hashPlan(a), hashPlan(c)
		if ha == hc {
			t.Fatal("seed does not affect the sample")
		}
	}
}

func hashPlan(p *Plan) string {
	h := sha256.New()
	for _, r := range p.Records {
		h.Write(r.Bytes)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestStreamsParse(t *testing.T) {
	for _, o := range []Opts{
		OpusStream(42, 120_000_000, 10_000_000, 2_000_000),
		AACStream(42, 120_000_000, 10_000_000, 2_000_000),
	} {
		if err := VerifySelf(o); err != nil {
			t.Fatalf("%s: %v", o.Codec, err)
		}
		plan, _, err := Generate(o)
		if err != nil {
			t.Fatal(err)
		}
		// Every non-lost, non-corrupt payload yields the declared duration.
		for _, p := range plan.Packets {
			if p.Lost || p.Corrupt {
				continue
			}
			if p.DurationUS != o.PacketUS {
				t.Fatalf("packet duration = %d", p.DurationUS)
			}
		}
		if plan.CoverageUS < o.DurationUS {
			t.Fatalf("coverage %d < duration %d", plan.CoverageUS, o.DurationUS)
		}
	}
}

func TestAACPacketDoesNotDivideGrid(t *testing.T) {
	if (21333*3)%10_000_000 == 0 {
		t.Fatal("test premise broken")
	}
	if 1024*1_000_000/48000 != 21333 {
		t.Fatal("AAC frame duration changed")
	}
}

func TestFaultSetsDisjointEnough(t *testing.T) {
	f := MixedFaults(42, 6000)
	if len(f.Lost) == 0 || len(f.Corrupt) == 0 || len(f.Duplicate) == 0 || len(f.Reorder) == 0 || len(f.Late) == 0 {
		t.Fatalf("faults missing: %+v", f)
	}
	if f.LateByUS < 0 || f.JitterUS < 0 {
		t.Fatal("negative fault timing")
	}
}

func TestGenerateRejectsBadOptions(t *testing.T) {
	bad := []Opts{
		{},
		{Codec: model.CodecOpus, ClockRate: 48000, PacketUS: 20000, TicksPerPacket: 960},
	}
	for i, o := range bad {
		if _, _, err := Generate(o); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	good := OpusStream(1, 1, 10, 0)
	if _, _, err := Generate(good); err == nil {
		t.Fatal("too-short duration accepted")
	}
}

func TestSilencePayloadDurations(t *testing.T) {
	o := AACStream(1, 10_000_000, 10_000_000, 0)
	p, err := SilencePayload(o)
	if err != nil {
		t.Fatal(err)
	}
	d, err := o.AAC.AACDurationUS(p)
	if err != nil || d <= 0 {
		t.Fatalf("silence aac = %d/%v", d, err)
	}
	po := OpusStream(1, 10_000_000, 10_000_000, 0)
	p, _ = SilencePayload(po)
	if d, err := codec.OpusDurationUS(p); err != nil || d != po.PacketUS {
		t.Fatalf("silence opus = %d/%v", d, err)
	}
}
