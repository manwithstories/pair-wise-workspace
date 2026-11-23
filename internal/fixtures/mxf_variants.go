package fixtures

import "fmt"

// BuildMXFInvalidKLV corrupts the first essence KLV length so it claims 2 GiB.
func BuildMXFInvalidKLV(o BuildMxfOptions) (*MxfBundle, error) {
	b, err := BuildMXF(o)
	if err != nil {
		return nil, err
	}
	idx := indexOfBytes(b.Data, ulEssence)
	if idx < 0 {
		return nil, fmt.Errorf("fixtures: essence key not found")
	}
	pos := idx + 16
	b.Data[pos] = 0x84
	copy(b.Data[pos+1:], []byte{0x7f, 0xff, 0xff, 0xff})
	b.Name = "invalid-klv.mxf"
	return b, nil
}

// BuildMXFTruncated cuts the stream inside the essence area.
func BuildMXFTruncated(o BuildMxfOptions) (*MxfBundle, error) {
	b, err := BuildMXF(o)
	if err != nil {
		return nil, err
	}
	idx := indexOfBytes(b.Data, ulEssence)
	if idx < 0 {
		return nil, fmt.Errorf("fixtures: essence key not found")
	}
	cut := idx + 16 + 8 + 64
	if cut >= len(b.Data) {
		cut = len(b.Data) - 32
	}
	b.Data = b.Data[:cut]
	b.Name = "truncated.mxf"
	b.TruncatedAt = int64(cut)
	return b, nil
}

func indexOfBytes(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
