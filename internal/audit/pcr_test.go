package audit

import (
	"math"
	"testing"

	"tsaudit/internal/ts"
)

func pcrPacket(pid uint16, base int64) *ts.Packet {
	return &ts.Packet{Header: ts.Header{PID: pid, AdaptationField: true}, Adapt: ts.Adaptation{HasPCR: true, PCRBase: base}}
}

func TestPCRWrapAndIntervals(t *testing.T) {
	c := NewPCRCheck()
	first := int64(1<<33) - 9000
	c.Observe(pcrPacket(0x101, first))
	c.Observe(pcrPacket(0x101, 0))
	c.Observe(pcrPacket(0x101, 9000))
	st := c.Stats()
	if len(st) != 1 || st[0].WrapCount != 1 || st[0].Count != 3 {
		t.Fatalf("unexpected PCR stats: %+v", st)
	}
	if st[0].FirstPCR != first || st[0].LastPCR != 9000 {
		t.Fatalf("unexpected PCR bounds: %+v", st[0])
	}
	if math.Abs(st[0].MeanIntervalMS-100) > 0.001 || st[0].MaxJitterMS > 0.001 {
		t.Fatalf("unexpected interval stats: %+v", st[0])
	}
}
