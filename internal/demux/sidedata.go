package demux

//#cgo pkg-config: libavutil
//#include <libavutil/pixdesc.h>
//#include <libavutil/pixfmt.h>
import "C"

import (
	"github.com/asticode/go-astiav"
)

// noDtsValue is the sentinel FFmpeg uses for a missing decode timestamp.
const noDtsValue = astiav.NoPtsValue

// colorPrimariesName returns the canonical lowercase name for a primaries
// enum, or "" when the stream does not specify one.
func colorPrimariesName(p astiav.ColorPrimaries) string {
	if p == astiav.ColorPrimariesUnspecified {
		return ""
	}
	return C.GoString(C.av_color_primaries_name(C.enum_AVColorPrimaries(p)))
}

// colorTransferName returns the canonical lowercase transfer name.
func colorTransferName(t astiav.ColorTransferCharacteristic) string {
	if t == astiav.ColorTransferCharacteristicUnspecified {
		return ""
	}
	return C.GoString(C.av_color_transfer_name(C.enum_AVColorTransferCharacteristic(t)))
}

// colorSpaceName returns the canonical lowercase matrix/space name.
func colorSpaceName(s astiav.ColorSpace) string {
	if s == astiav.ColorSpaceUnspecified {
		return ""
	}
	return C.GoString(C.av_color_space_name(C.enum_AVColorSpace(s)))
}

// colorRangeName returns "tv" for limited range and "pc" for full range, else
// "" when unspecified.
func colorRangeName(r astiav.ColorRange) string {
	switch r {
	case astiav.ColorRangeMpeg:
		return "tv"
	case astiav.ColorRangeJpeg:
		return "pc"
	default:
		return ""
	}
}

// rotationOf reads the display matrix from a stream's codec parameters side
// data and returns the clockwise rotation in whole degrees. A missing matrix
// yields 0, false.
func rotationOf(cp *astiav.CodecParameters) (int, bool) {
	if cp == nil {
		return 0, false
	}
	dm, ok := cp.SideData().DisplayMatrix().Get()
	if !ok {
		return 0, false
	}
	deg := dm.Rotation()
	r := int(deg + 0.5)
	if deg < 0 {
		r = -int(-deg + 0.5)
	}
	r = ((r % 360) + 360) % 360
	return r, true
}
