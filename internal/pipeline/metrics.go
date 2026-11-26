package pipeline

import (
	"sort"

	"mediascan/internal/media"
)

func sortFaults(faults []media.Fault) {
	sort.SliceStable(faults, func(i, j int) bool {
		a, b := faults[i], faults[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.TrackID != b.TrackID {
			return a.TrackID < b.TrackID
		}
		if a.ByteOffset != b.ByteOffset {
			return a.ByteOffset < b.ByteOffset
		}
		return a.Code < b.Code
	})
}

func countFaults(faults []media.Fault, match func(string) bool) int {
	n := 0
	for _, f := range faults {
		if match(f.Code) {
			n++
		}
	}
	return n
}

func countFaultCode(faults []media.Fault, code string) int {
	n := 0
	for _, f := range faults {
		if f.Code == code {
			n++
		}
	}
	return n
}
