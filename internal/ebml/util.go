package ebml

import "math"

func readUint(b []byte) (uint64, bool) {
	if len(b) == 0 || len(b) > 8 {
		return 0, false
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, true
}

func readFloat(b []byte) float64 {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(readUint32(b)))
	case 8:
		return math.Float64frombits(readUint64(b))
	}
	return 0
}

func readUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func readUint64(b []byte) uint64 {
	return uint64(readUint32(b[:4]))<<32 | uint64(readUint32(b[4:]))
}

func trimZeros(b []byte) string {
	for len(b) > 0 && (b[len(b)-1] == 0 || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return string(b)
}
