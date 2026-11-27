package recover

import (
	"mediacheck/internal/model"
)

// Method identifies how a missing slot was filled.
type Method uint8

const (
	// MethodNone means no fill was attempted.
	MethodNone Method = iota
	// MethodFEC used Opus in-band redundancy.
	MethodFEC
	// MethodPLC used libopus packet loss concealment.
	MethodPLC
	// MethodSilence inserted an encoded silence frame.
	MethodSilence
	// MethodGiveUp means every method failed; the slot stays missing.
	MethodGiveUp
)

// String renders the method for report output.
func (m Method) String() string {
	switch m {
	case MethodFEC:
		return "fec"
	case MethodPLC:
		return "plc"
	case MethodSilence:
		return "silence"
	case MethodGiveUp:
		return "none"
	default:
		return "none"
	}
}

// FaultKind maps the method to the fault kind recorded for the slot.
func (m Method) FaultKind() model.FaultKind {
	switch m {
	case MethodFEC:
		return model.FaultFECRecovered
	case MethodPLC:
		return model.FaultPLCReplaced
	case MethodSilence:
		return model.FaultSilenceFill
	default:
		return model.FaultLoss
	}
}

// Capabilities describes which recovery paths are available in this build.
type Capabilities struct {
	// FEC is true when libopus and an Opus stream are in use.
	FEC bool
	// PLC is true when a libopus decoder could be created.
	PLC bool
	// Silence is always available and only needs the encoder.
	Silence bool
}

// SlotResult reports what happened to one missing slot.
type SlotResult struct {
	Slot   model.Slot
	Method Method
	Data   []byte // encoded replacement packet, nil when MethodGiveUp
	Reason string // short human explanation for report output
}
