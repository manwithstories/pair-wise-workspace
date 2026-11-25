package segment

import (
	"medpipe/internal/clock"
	"medpipe/internal/mp4"
)

// ColorPolicy decides whether source color metadata may be applied to an
// output. A mismatch between the source track and an incoming override is an
// error: conflicts must never be silently overwritten.
type ColorPolicy struct {
	// Override is an externally supplied tag set (may be nil).
	Override *mp4.ColorMeta
}

// Resolve returns the metadata to write. When both source and override exist
// and differ, it returns KindInconsistent so the caller aborts the segment.
func (cp ColorPolicy) Resolve(source *mp4.ColorMeta) (*mp4.ColorMeta, error) {
	if cp.Override == nil {
		return source, nil // preserve source as-is, including nil
	}
	if source == nil {
		return cp.Override, nil
	}
	if !equalColor(*source, *cp.Override) {
		return nil, clock.E(clock.KindInconsistent, "segment.color",
			clock.WithDetail("color metadata conflict: source=%+v override=%+v",
				*source, *cp.Override))
	}
	return source, nil
}

func equalColor(a, b mp4.ColorMeta) bool {
	return a.ColorType == b.ColorType &&
		a.ColourPrimaries == b.ColourPrimaries &&
		a.TransferCharacteristics == b.TransferCharacteristics &&
		a.MatrixCoefficients == b.MatrixCoefficients &&
		a.FullRange == b.FullRange
}

// ffmpegColorArgs maps preserved color metadata onto ffmpeg muxer flags so
// the output container carries the same nclx tags as the source.
func ffmpegColorArgs(c *mp4.ColorMeta) []string {
	if c == nil || c.ColorType != "nclx" {
		return nil
	}
	return []string{
		"-color_primaries", nclxName(primariesNames, c.ColourPrimaries),
		"-color_trc", nclxName(transferNames, c.TransferCharacteristics),
		"-colorspace", nclxName(matrixNames, c.MatrixCoefficients),
	}
}

// nclxName falls back to the numeric index when no symbolic name is known.
func nclxName(names map[uint16]string, idx uint16) string {
	if name, ok := names[idx]; ok {
		return name
	}
	return uintToString(idx)
}

var primariesNames = map[uint16]string{
	1: "bt709", 9: "bt2020", 6: "smpte170m", 5: "smpte240m", 11: "smpte432",
}

var transferNames = map[uint16]string{
	1: "bt709", 16: "smpte2084", 18: "smpte428", 8: "linear", 6: "smpte170m",
}

var matrixNames = map[uint16]string{
	1: "bt709", 9: "bt2020nc", 6: "smpte170m", 5: "smpte240m",
}

func uintToString(v uint16) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
