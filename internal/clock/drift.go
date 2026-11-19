package clock

import "archivearchivist/internal/model"

// estimateDrift is implemented with the drift fit in the next change.
func estimateDrift(points []Point, rate uint32) model.Rational {
	return model.NewRat(0, 1)
}

// measureSkew is implemented with the drift fit in the next change.
func measureSkew(points []Point, rate uint32, ppm model.Rational) int64 {
	return 0
}
