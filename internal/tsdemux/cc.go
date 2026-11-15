package tsdemux

// NullPID is the MPEG-TS null packet PID.
const NullPID = 0x1FFF

const ccUnseen = int16(-1)

// CCTracker tracks the continuity counter state machine per PID.
//
// Rules:
//   - packets without payload (adaptation-only) do not advance CC
//   - discontinuity_indicator resets expectation without counting loss
//   - a repeated CC with payload counts as a duplicate
//   - 15 -> 0 wrap is the normal case, not an error
//   - any other jump estimates the number of lost packets
type CCTracker struct {
	last  [8192]int16
	jumps [8192]int64
	lost  [8192]int64
	dup   [8192]int64
}

func NewCCTracker() *CCTracker {
	t := &CCTracker{}
	for i := range t.last {
		t.last[i] = ccUnseen
	}
	return t
}

// Observe feeds one packet into the per-PID CC state machine.
func (t *CCTracker) Observe(p *Packet) {
	if p.PID == NullPID || p.TEI || !p.HasPayload {
		return
	}
	pid := p.PID
	if p.Discontinuity {
		t.last[pid] = int16(p.CC)
		return
	}
	last := t.last[pid]
	if last == ccUnseen {
		t.last[pid] = int16(p.CC)
		return
	}
	cc := int16(p.CC)
	expected := (last + 1) & 0xF
	switch cc {
	case last:
		t.dup[pid]++
	case expected:
	default:
		t.jumps[pid]++
		t.lost[pid] += int64((cc - expected) & 0xF)
	}
	if cc != last {
		t.last[pid] = cc
	}
}

// Stats returns jumps, estimated lost packets and duplicates for a PID.
func (t *CCTracker) Stats(pid uint16) (jumps, lost, dup int64) {
	return t.jumps[pid], t.lost[pid], t.dup[pid]
}

// Totals sums jumps, lost and dup over all PIDs.
func (t *CCTracker) Totals() (jumps, lost, dup int64) {
	for pid := 0; pid < 8192; pid++ {
		jumps += t.jumps[pid]
		lost += t.lost[pid]
		dup += t.dup[pid]
	}
	return
}
