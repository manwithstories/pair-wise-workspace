package metadata

import (
	"fmt"
	"math"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
)

// WriteOptions describes one track to be serialized by the MP4 writer.
// It is data-only so the same generator drives fixtures and demos.
type WriteOptions struct {
	ContainerColor ColorDesc
	Tracks         []WriteTrack
}

// WriteTrack is the fixture-level description of a track. Timing deltas are
// in SampleDelta units; each sample has a fixed byte size.
type WriteTrack struct {
	Kind        Kind
	Timescale   int
	Duration    int64
	SampleDelta int64
	SampleCount int

	Width, Height int
	Matrix        [9]int32
	Crop          *Crop
	Color         ColorDesc

	SampleRate int
	Channels   int
}

// MatrixForRotation returns the ISO matrix for a clockwise display rotation.
func MatrixForRotation(deg int) [9]int32 {
	const one = 1 << 16
	switch deg {
	case 90:
		return [9]int32{0, one, 0, -one, 0, 0, 0, 0, 1 << 30}
	case 180:
		return [9]int32{-one, 0, 0, 0, -one, 0, 0, 0, 1 << 30}
	case 270:
		return [9]int32{0, -one, 0, one, 0, 0, 0, 0, 1 << 30}
	default:
		return [9]int32{one, 0, 0, 0, one, 0, 0, 0, 1 << 30}
	}
}

// RotationForMatrix recovers a right-angle rotation from an ISO matrix and
// reports the implied angle for anything else, letting NormalizeRotation
// reject it with track and span context.
func RotationForMatrix(trackID int, m [9]int32, span clock.Span) (int, error) {
	for _, deg := range []int{0, 90, 180, 270} {
		if m == MatrixForRotation(deg) {
			return deg, nil
		}
	}
	ang := math.Atan2(float64(m[1]), float64(m[0])) * 180 / math.Pi
	for ang < 0 {
		ang += 360
	}
	return 0, fault.NewSpan(fault.StageResolve, fault.CodeRotationInvalid, trackID, span,
		fmt.Sprintf("track matrix implies %.3f degrees", ang))
}
