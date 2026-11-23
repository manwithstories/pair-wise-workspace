package pipe

import (
	"fmt"

	"archivecheck/internal/fault"
	"archivecheck/internal/index"
	"archivecheck/internal/verify"
)

// ValidateResumeState proves that a checkpoint still describes the bytes on
// disk before any work continues. It re-reads the committed output prefix and
// requires it to decode to exactly the checkpoint's index, with the index
// internally consistent. Any mismatch is an incompatible-resume fault, so the
// caller exits 3 without overwriting the old artifacts.
func ValidateResumeState(outPath string, cp *index.Checkpoint) error {
	if cp == nil {
		return fault.New(fault.KindCheckpoint, "", "", 0, "nil checkpoint")
	}
	if cp.Committed > 0 {
		scan, err := verify.ScanFile(outPath, cp.OutputBytes)
		if err != nil {
			return fault.Wrap(err, fault.KindCheckpoint, outPath, "", cp.OutputBytes,
				"committed output prefix cannot be re-read")
		}
		reRead, err := index.Normalize(scan.Entries)
		if err != nil {
			return fault.Wrap(err, fault.KindCheckpoint, outPath, "", cp.OutputBytes,
				"committed output prefix does not decode to a valid index")
		}
		if cp.Index == nil {
			return fault.New(fault.KindCheckpoint, outPath, "", cp.OutputBytes,
				"checkpoint has no index for a non-empty committed prefix")
		}
		if len(reRead.Entries) != len(cp.Index.Entries) {
			return fault.New(fault.KindCheckpoint, outPath, "", cp.OutputBytes,
				fmt.Sprintf("checkpoint index has %d entries, committed output has %d",
					len(cp.Index.Entries), len(reRead.Entries)))
		}
		for i := range reRead.Entries {
			x, y := cp.Index.Entries[i], reRead.Entries[i]
			if x.TrackID != y.TrackID || x.DTS != y.DTS || x.EndDTS != y.EndDTS ||
				x.Offset != y.Offset || x.Size != y.Size || x.Segment != y.Segment {
				return fault.New(fault.KindCheckpoint, y.Segment, "", y.Offset,
					fmt.Sprintf("checkpoint index entry %d disagrees with committed output", i))
			}
		}
	}
	if cp.Index != nil {
		if err := cp.Index.Validate(cp.OutputBytes); err != nil {
			return fault.Wrap(err, fault.KindCheckpoint, outPath, "", cp.OutputBytes,
				"checkpoint index fails structural validation")
		}
	}
	return nil
}
