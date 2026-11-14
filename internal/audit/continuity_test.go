package audit

import (
	"testing"

	"tsaudit/internal/ts"
)

func ccPacket(pid uint16, cc uint8, payload bool, disi bool) *ts.Packet {
	p := &ts.Packet{Header: ts.Header{PID: pid, HasPayload: payload, ContinuityCounter: cc}}
	if disi {
		p.Header.AdaptationField = true
		p.Adapt.DiscontinuityIndicator = true
	}
	return p
}

func TestContinuityStateMachine(t *testing.T) {
	c := NewContinuity()
	for _, cc := range []uint8{0, 1, 2, 5, 2, 2, 9, 10} {
		c.Observe(ccPacket(0x101, cc, true, cc == 9))
	}
	c.Observe(ccPacket(ts.NullPID, 0, true, false))
	c.Observe(ccPacket(0x102, 4, false, false))
	st := c.Stats()
	if st.LossPackets != 2 || st.CCJumps != 1 || st.Duplicates != 1 || st.Discontinuities != 1 {
		t.Fatalf("unexpected continuity stats: %+v", st)
	}
	if len(st.PerPID) != 1 || st.PerPID[0].Packets != 8 {
		t.Fatalf("unexpected per-PID stats: %+v", st.PerPID)
	}
}

func TestContinuityAdaptationOnlyDiscontinuityResets(t *testing.T) {
	c := NewContinuity()
	c.Observe(ccPacket(0x101, 3, true, false))
	c.Observe(ccPacket(0x101, 4, false, true))
	c.Observe(ccPacket(0x101, 5, true, false))
	st := c.Stats()
	if st.LossPackets != 0 || st.CCJumps != 0 || st.Discontinuities != 1 {
		t.Fatalf("unexpected stats: %+v", st)
	}
}
