package fixtures

func identityMatrix() []byte {
	m := []int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
	return matrixBytes(m)
}

func rotationMatrix(deg int) []byte {
	switch deg {
	case 90:
		return matrixBytes([]int32{0, 0x10000, 0, -0x10000, 0, 0, 0, 0, 0x40000000})
	case 180:
		return matrixBytes([]int32{-0x10000, 0, 0, 0, -0x10000, 0, 0, 0, 0x40000000})
	case 270:
		return matrixBytes([]int32{0, -0x10000, 0, 0x10000, 0, 0, 0, 0, 0x40000000})
	default:
		return identityMatrix()
	}
}

func matrixBytes(m []int32) []byte {
	out := make([]byte, 0, 36)
	for _, v := range m {
		out = append(out, be32(uint32(v))...)
	}
	return out
}

func sizeList(seed int64, track uint32, count, base, spread int) []byte {
	out := make([]byte, 0, count*4)
	for i := 0; i < count; i++ {
		out = append(out, be32(uint32(sampleSize(seed, track, i, base, spread)))...)
	}
	return out
}

func makeAvcC() []byte {
	// Minimal avcC: baseline profile, one SPS and one PPS, lengthSize=4.
	p := []byte{1, 0x42, 0x00, 0x1e, 0xff, 0xe1}
	sps := []byte{0x67, 0x42, 0x00, 0x1e, 0x95, 0xa8, 0x14, 0x01, 0x6e, 0x9b, 0x80}
	p = append(p, be16(uint16(len(sps)))...)
	p = append(p, sps...)
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	p = append(p, 1)
	p = append(p, be16(uint16(len(pps)))...)
	p = append(p, pps...)
	return box(nil, "avcC", p)
}
