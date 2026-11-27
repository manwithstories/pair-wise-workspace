package pipeline

import (
	"fmt"

	"relayaudit/internal/checkpoint"
	"relayaudit/internal/fault"
	"relayaudit/internal/timeline"
	"relayaudit/internal/transform"
	"relayaudit/internal/wire"
)

const DefaultBatchRows = 256

// Config selects the source and the run behaviour.
type Config struct {
	// Source is the full segment bytes.
	Source []byte
	// SourceSHA256 pins checkpoints to this input; computed when empty.
	SourceSHA256 string
	// Seed feeds deterministic proxy rendering.
	Seed int64
	// BatchRows overrides DefaultBatchRows.
	BatchRows int
	// InterruptAfter simulates an abrupt stop after this many scanned
	// records have been consumed. Zero disables the simulation. The
	// checkpoint committed at that point stays valid.
	InterruptAfter int
	// Resume continues from the store's committed checkpoint.
	Resume bool
	// Store receives checkpoints; required.
	Store *checkpoint.Store
}

// Outcome is the result of a run. It is returned even for a simulated
// interruption so the caller can report partial progress.
type Outcome struct {
	Scan     *wire.ScanResult
	Timeline *timeline.Resolution
	// BoundaryFaults are timeline faults at record-interior clip edges.
	BoundaryFaults []*fault.Fault
	// Ranges are the isolated source spans.
	Ranges []transform.Range
	// Output is the final repaired file. Nil when interrupted.
	Output []byte
	// OutputSHA256 is the digest of Output.
	OutputSHA256 string
	// Kept and Dropped record counts.
	Kept, Dropped int
	// NextIndex is the number of source records consumed.
	NextIndex int
	// IndexRebuilt reports that the source index had to be rebuilt.
	IndexRebuilt bool
	// Resumed reports that the run continued from a checkpoint.
	Resumed bool
	// Interrupted reports a simulated stop with a committed checkpoint.
	Interrupted bool
	// AlreadyComplete reports the store already held a finished result.
	AlreadyComplete bool
	// ResumeMillis is the time spent after restoring the checkpoint.
	ResumeMillis int64
	// ProxyHash is recomputed by verification, exposed for summaries.
	ProxyHash string
}

// ErrInterrupted signals a simulated interruption that left a valid
// checkpoint behind. Callers should resume.
var ErrInterrupted = fmt.Errorf("run interrupted with committed checkpoint")
