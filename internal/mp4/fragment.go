package mp4

import (
	"mediad/internal/core"
)

// Trun/tfhd/sample flag bits used by this package.
const (
	FlagDataOffsetPresent uint32 = 0x000001
	FlagFirstSampleFlags  uint32 = 0x000004
	FlagSampleDuration    uint32 = 0x000100
	FlagSampleSize        uint32 = 0x000200
	FlagSampleFlags       uint32 = 0x000400
	FlagSampleCTS         uint32 = 0x000800
	FlagNonSyncSample     uint32 = 0x00010000

	TFHDDefaultBaseIsMoof uint32 = 0x020000
)

// Sample is one trun entry. CTOffset is the normalized signed composition time
// offset; the raw encoding version is retained on Fragment for roundtrip.
type Sample struct {
	Duration uint32
	Size     uint32
	Flags    uint32
	CTOffset int32
}

// IsSync reports whether the sample's non-sync bit is clear, i.e. it is a
// sync/keyframe sample.
func (s Sample) IsSync() bool { return s.Flags&FlagNonSyncSample == 0 }

// Fragment is the fully decoded mfhd/tfdt/trun view plus the mdat payload.
type Fragment struct {
	Sequence      uint32
	TrackID       uint32
	BaseDecode    uint64
	TFDTVersion   byte
	TrunVersion   byte
	TrunFlags     uint32
	DataOffset    int32
	HasDataOffset bool
	Samples       []Sample
	Data          []byte
}

// KeyframeCount returns the number of sync samples in the trun.
func (f *Fragment) KeyframeCount() int {
	n := 0
	for _, s := range f.Samples {
		if s.IsSync() {
			n++
		}
	}
	return n
}

// readNeed ensures n bytes can be read at p before limit (the enclosing box
// end). Failures distinguish an out-of-container reference from true EOF.
func readNeed(data []byte, p, limit int64, n int, what string) error {
	if n < 0 || p < 0 {
		return core.Faultf(core.KindOutOfBounds, p, "negative position for %s", what)
	}
	end, ok := core.AddChecked(p, int64(n))
	if !ok {
		return core.Faultf(core.KindOverflow, p, "offset overflow reading %s", what)
	}
	if end > limit {
		kind := core.KindOutOfBounds
		if end > int64(len(data)) {
			kind = core.KindTruncated
		}
		return core.Faultf(kind, p, "reading %s needs %d bytes up to %d, limit %d", what, n, end, limit)
	}
	return nil
}
