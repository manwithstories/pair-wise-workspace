package sample

import "medialab/internal/format"

// MXFFixtures contains normal KLV and a deterministically truncated KLV.
type MXFFixtures struct {
	Normal          []byte
	TruncatedKLV    []byte
	TruncatedOffset int64
}

// BuildMXFFixtures constructs the MXF metadata subset samples.
func BuildMXFFixtures() MXFFixtures {
	containerColor := format.KLV{Key: format.MXFKeyFor(format.MXFTagContainerColor), Value: format.ColorValue(9, 16, 9)}
	videoID := format.KLV{Key: format.MXFKeyFor(format.MXFTagTrackID), Value: format.Uint32Value(11)}
	kind := format.KLV{Key: format.MXFKeyFor(format.MXFTagKind), Value: []byte("vide")}
	time := format.KLV{Key: format.MXFKeyFor(format.MXFTagTimescale), Value: format.Uint32Value(30000)}
	dimensions := format.KLV{Key: format.MXFKeyFor(format.MXFTagDimensions), Value: append(format.Uint32Value(640), format.Uint32Value(360)...)}
	rotation := format.KLV{Key: format.MXFKeyFor(format.MXFTagRotation), Value: format.Int32Value(180)}
	cropValue := append(append(append(format.Int32Value(16), format.Int32Value(8)...), format.Int32Value(16)...), format.Int32Value(8)...)
	crop := format.KLV{Key: format.MXFKeyFor(format.MXFTagCrop), Value: cropValue}
	color := format.KLV{Key: format.MXFKeyFor(format.MXFTagTrackColor), Value: format.ColorValue(1, 1, 1)}
	keyframeValue := append(append(format.Uint32Value(1), format.Uint32Value(5)...), format.Uint32Value(9)...)
	keyframes := format.KLV{Key: format.MXFKeyFor(format.MXFTagKeyframes), Value: keyframeValue}
	audioID := format.KLV{Key: format.MXFKeyFor(format.MXFTagTrackID), Value: format.Uint32Value(12)}
	audioKind := format.KLV{Key: format.MXFKeyFor(format.MXFTagKind), Value: []byte("soun")}
	normal := format.BuildMXF(containerColor, videoID, kind, time, dimensions, rotation, crop, color, keyframes, audioID, audioKind)

	prefix := videoID.Encode()
	fullDimensions := dimensions.Encode()
	// Keep key(16)+BER length(1)+only half of the 8-byte value. The BER still
	// advertises eight bytes, so ParseMXF reports this KLV's original offset.
	truncated := append(append([]byte(nil), prefix...), fullDimensions[:16+1+4]...)
	return MXFFixtures{Normal: normal, TruncatedKLV: truncated, TruncatedOffset: int64(len(prefix))}
}
