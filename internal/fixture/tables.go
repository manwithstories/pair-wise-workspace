package fixture

import "encoding/binary"

func (b *builder) stbl(lay trackLayout, mdatStart int64) []byte {
	stsd := b.stsd(lay)
	var children [][]byte
	children = append(children, stsd)
	if !b.spec.Fault.MissingIndex {
		children = append(children, b.stts(lay))
		if lay.spec.Kind == "video" {
			children = append(children, b.stss(lay))
		}
		children = append(children, b.stsc())
		children = append(children, b.stsz(lay))
		children = append(children, b.stco(lay, mdatStart))
	} else {
		// Keep only timing so a player knows the cadence; byte layout is lost.
		children = append(children, b.stts(lay))
	}
	return containerBox("stbl", children...)
}

func (b *builder) stts(lay trackLayout) []byte {
	p := binary.BigEndian.AppendUint32(nil, 1) // entry count
	p = binary.BigEndian.AppendUint32(p, uint32(lay.spec.Count))
	p = binary.BigEndian.AppendUint32(p, lay.spec.Delta)
	return fullbox("stts", 0, 0, p)
}

func (b *builder) stss(lay trackLayout) []byte {
	p := binary.BigEndian.AppendUint32(nil, uint32(len(lay.syncs)))
	for _, n := range lay.syncs {
		p = binary.BigEndian.AppendUint32(p, n)
	}
	return fullbox("stss", 0, 0, p)
}

func (b *builder) stsc() []byte {
	p := binary.BigEndian.AppendUint32(nil, 1)
	p = binary.BigEndian.AppendUint32(p, 1) // first chunk
	p = binary.BigEndian.AppendUint32(p, 1) // one sample per chunk
	p = binary.BigEndian.AppendUint32(p, 1) // sample description index
	return fullbox("stsc", 0, 0, p)
}

func (b *builder) stsz(lay trackLayout) []byte {
	p := binary.BigEndian.AppendUint32(nil, 0) // uniform size 0 => per-sample
	p = binary.BigEndian.AppendUint32(p, uint32(lay.spec.Count))
	for range lay.spec.Count {
		p = binary.BigEndian.AppendUint32(p, uint32(lay.spec.SampleSize))
	}
	return fullbox("stsz", 0, 0, p)
}

func (b *builder) stco(lay trackLayout, mdatStart int64) []byte {
	p := binary.BigEndian.AppendUint32(nil, uint32(len(lay.offsets)))
	for _, off := range lay.offsets {
		p = binary.BigEndian.AppendUint32(p, uint32(int64(off)+mdatStart))
	}
	return fullbox("stco", 0, 0, p)
}
