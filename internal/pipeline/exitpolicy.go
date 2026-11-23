package pipeline

import (
	"mediasim/internal/mp4"
)

// decideExit implements the contract: 0 complete, 1 failed, 2 partial.
func decideExit(rep *Report, expected int, info *mp4.Info) int {
	if rep.Frames > 0 && len(info.Gaps) > 0 {
		return ExitPartial
	}
	if expected == 0 || rep.Frames == expected {
		if rep.CorruptRanges == 0 && rep.Gates.AVSyncOK && rep.Gates.RecoveryOK && rep.Gates.LatencyOK {
			return ExitOK
		}
	}
	if rep.Frames > 0 && rep.Gates.AVSyncOK && rep.Gates.LatencyOK {
		return ExitPartial
	}
	return ExitError
}
