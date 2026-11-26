package media

import "fmt"

// FaultCode identifies the kind of fault detected by a subsystem. Faults keep
// their origin code so that an ingest parse error can be followed through
// track dependency handling all the way to index publication.
type FaultCode string

const (
	FaultNone FaultCode = ""

	// Ingest faults.
	FaultTruncated     FaultCode = "truncated_box"      // box header/payload runs past EOF
	FaultBadLength     FaultCode = "invalid_box_length" // declared length smaller than header
	FaultSampleRange   FaultCode = "sample_range"       // trun samples exceed mdat payload
	FaultOverflow      FaultCode = "arithmetic_overflow"
	FaultStructure     FaultCode = "structure"           // required box missing or duplicated

	// Timeline faults.
	FaultSequenceGap   FaultCode = "mfhd_gap"        // repaired, kept for diagnostics
	FaultDuplicate     FaultCode = "duplicate_fragment"
	FaultConflict      FaultCode = "sequence_conflict"
	FaultTFDTRollback  FaultCode = "tfdt_rollback"
	FaultUnrepairable  FaultCode = "unrepairable_track"
	FaultGraphCycle    FaultCode = "dependency_cycle"
	FaultMissingDep    FaultCode = "missing_dependency"
	FaultDepFailed     FaultCode = "dependency_failed"

	// Stream / persistence faults.
	FaultQueueLimit    FaultCode = "queue_limit"
	FaultCheckpoint    FaultCode = "checkpoint_io"
	FaultHashMismatch  FaultCode = "hash_mismatch"
)

// Fault is the unified error type shared by every subsystem. It always
// carries the fragment number, track id and byte offset involved; fields
// that do not apply to a particular fault are left zero.
type Fault struct {
	Code     FaultCode
	TrackID  uint32
	Sequence uint32 // mfhd sequence number, 0 when unknown
	Offset   int64  // first erroneous byte offset in the source stream
	Detail   string
}

// Error implements error.
func (f *Fault) Error() string {
	switch {
	case f.TrackID != 0 && f.Sequence != 0:
		return fmt.Sprintf("media fault %s: track=%d seq=%d offset=%d: %s",
			f.Code, f.TrackID, f.Sequence, f.Offset, f.Detail)
	case f.TrackID != 0:
		return fmt.Sprintf("media fault %s: track=%d offset=%d: %s",
			f.Code, f.TrackID, f.Offset, f.Detail)
	default:
		return fmt.Sprintf("media fault %s: offset=%d: %s", f.Code, f.Offset, f.Detail)
	}
}

// NewFault builds a Fault with all positional fields.
func NewFault(code FaultCode, trackID, sequence uint32, offset int64, detail string) *Fault {
	return &Fault{Code: code, TrackID: trackID, Sequence: sequence, Offset: offset, Detail: detail}
}

// Is lets errors.Is match faults by code as well as by concrete value.
func (f *Fault) Is(target error) bool {
	t, ok := target.(*Fault)
	if !ok {
		return false
	}
	return t.Code == FaultNone || t.Code == f.Code
}

// Isolation reports whether this fault requires the owning fragment or
// track to be quarantined (kept out of the repaired timeline and index).
func (f *Fault) Isolation() bool {
	switch f.Code {
	case FaultTruncated, FaultBadLength, FaultSampleRange, FaultOverflow,
		FaultStructure, FaultConflict, FaultUnrepairable, FaultGraphCycle,
		FaultMissingDep, FaultDepFailed, FaultHashMismatch:
		return true
	default:
		return false
	}
}
