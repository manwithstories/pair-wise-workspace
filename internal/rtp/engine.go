package rtp

import (
	"fmt"

	"medialab/internal/model"
)

// MaxXORGroups bounds stored repair groups.
const MaxXORGroups = 128

// XORTable indexes parity groups by protected sequence number.
type XORTable struct {
	groups map[string]XORGroup
	bySeq  map[uint16][]string
}

// NewXORTable creates an empty bounded XOR table.
func NewXORTable() *XORTable {
	return &XORTable{groups: make(map[string]XORGroup), bySeq: make(map[uint16][]string)}
}

// Add stores one group and indexes all members.
func (t *XORTable) Add(g XORGroup) error {
	if len(t.groups) >= MaxXORGroups {
		return model.New(model.StageRecovery, "xor_table_full", 0, -1, fmt.Errorf("more than %d XOR groups", MaxXORGroups))
	}
	t.groups[g.ID] = g
	for _, m := range g.Members {
		t.bySeq[m.Sequence] = append(t.bySeq[m.Sequence], g.ID)
	}
	return nil
}

// Recover tries every group containing seq until one has exactly one missing.
func (t *XORTable) Recover(seq uint16, available map[uint16]*InPacket) (*InPacket, string, bool) {
	for _, id := range t.bySeq[seq] {
		g := t.groups[id]
		if pkt, ok := g.Recover(available); ok {
			return pkt, id, true
		}
	}
	return nil, "", false
}

// RecoveryResult records the path by which a lost sequence was handled.
type RecoveryResult struct {
	Seq     uint16
	State   model.RecoveryState
	Packet  *InPacket
	GroupID string
	Fault   *model.Fault
}

// Engine combines time-limited retransmission cache and XOR FEC.
type Engine struct {
	Cache         *RetransmitCache
	FEC           *XORTable
	Retransmitted int
	XORRecovered  int
	Unrecoverable int
}

// NewEngine creates the default bounded recovery engine.
func NewEngine(retransmitTTLMS int64) *Engine {
	return &Engine{
		Cache: NewRetransmitCache(retransmitTTLMS, MaxRetransmitCache),
		FEC:   NewXORTable(),
	}
}

// RecoverLost handles a deterministic loss list in sequence order. Cached
// retransmission is attempted first; XOR is the second recovery path.
func (e *Engine) RecoverLost(nowMS int64, lost []uint16, available map[uint16]*InPacket) []RecoveryResult {
	var results []RecoveryResult
	for _, seq := range lost {
		if pkt, ok := e.Cache.Get(seq, nowMS); ok {
			e.Retransmitted++
			available[seq] = pkt
			results = append(results, RecoveryResult{Seq: seq, State: model.StateRetransmitted, Packet: pkt})
			continue
		}
		if pkt, group, ok := e.FEC.Recover(seq, available); ok {
			e.XORRecovered++
			available[seq] = pkt
			results = append(results, RecoveryResult{Seq: seq, State: model.StateXORRecovered, Packet: pkt, GroupID: group})
			continue
		}
		e.Unrecoverable++
		f := model.New(model.StageRecovery, "burst_unrecoverable", 0, -1,
			fmt.Errorf("sequence %d missed retransmission and XOR recovery", seq))
		results = append(results, RecoveryResult{Seq: seq, State: model.StateDropped, Fault: f})
	}
	return results
}
