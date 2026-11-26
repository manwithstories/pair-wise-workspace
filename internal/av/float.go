package av

import (
	"encoding/binary"
	"math"
)

// PutFloat32LE writes v into b at off in little-endian order.
func PutFloat32LE(b []byte, off int, v float32) {
	binary.LittleEndian.PutUint32(b[off:off+4], math.Float32bits(v))
}

// Float32LE reads a little-endian float32 from b at off.
func Float32LE(b []byte, off int) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b[off : off+4]))
}
