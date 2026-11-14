// Package cc tracks per-PID continuity counters and estimates packet loss.
package cc

import "hash/crc32"

// Event describes one continuity-counter discontinuity.
type Event struct {
	PID      uint16 `json:"pid"`
	Offset   int64  `json:"offset"` // byte offset of the offending packet
	Expected byte   `json:"expected"`
	Got      byte   `json:"got"`
	Lost     int    `json:"lost"` // estimated missing packets (0 for backward jumps)
}

type pidState struct {
	seen     bool
	lastCC   byte
	lastHash uint32
}

// Tracker maintains per-PID CC state.
type Tracker struct {
	states map[uint16]*pidState

	JumpEvents   uint64 // anomalous jumps with no estimated loss
	LossEvents   uint64 // forward gaps with estimated loss
	Duplicates   uint64 // repeated CC with identical payload
	LostEstimate uint64 // estimated lost packets
	Events       []Event
}

// New creates a Tracker.
func New() *Tracker {
	return &Tracker{states: map[uint16]*pidState{}}
}

// Observe processes one packet. hasPayload/payload describe the TS payload;
// packets without payload do not advance the CC and are not checked.
func (t *Tracker) Observe(pid uint16, ccVal byte, discontinuity bool, hasPayload bool, payload []byte, offset int64) {
	if !hasPayload {
		return
	}
	st := t.states[pid]
	if st == nil {
		st = &pidState{}
		t.states[pid] = st
	}
	if !st.seen {
		st.seen = true
		st.lastCC = ccVal
		st.lastHash = crc32.ChecksumIEEE(payload)
		return
	}
	if discontinuity {
		// Encoder signalled a discontinuity: reset expectations.
		st.lastCC = ccVal
		st.lastHash = crc32.ChecksumIEEE(payload)
		return
	}
	expected := (st.lastCC + 1) & 0x0F
	switch {
	case ccVal == expected:
		// in order
	case ccVal == st.lastCC:
		// Possible duplicate: identical payload means a rebroadcast, not loss.
		if crc32.ChecksumIEEE(payload) == st.lastHash {
			t.Duplicates++
			return
		}
		t.record(pid, offset, expected, ccVal, 0)
	default:
		lost := int((ccVal - expected) & 0x0F)
		if lost > 8 {
			lost = 0 // backward jump: count the event, estimate no loss
		}
		t.record(pid, offset, expected, ccVal, lost)
	}
	st.lastCC = ccVal
	st.lastHash = crc32.ChecksumIEEE(payload)
}

func (t *Tracker) record(pid uint16, offset int64, expected, got byte, lost int) {
	if lost > 0 {
		t.LossEvents++
	} else {
		t.JumpEvents++
	}
	t.LostEstimate += uint64(lost)
	t.Events = append(t.Events, Event{
		PID: pid, Offset: offset, Expected: expected, Got: got, Lost: lost,
	})
}
