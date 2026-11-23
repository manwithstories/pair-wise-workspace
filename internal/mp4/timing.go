package mp4

func sttsTotal(d sttsData) int {
	total := 0
	for _, e := range d.entries {
		total += int(e.count)
	}
	return total
}

func expandStts(d sttsData, n int) ([]int64, []int64) {
	dts := make([]int64, n)
	dur := make([]int64, n)
	cur := int64(0)
	i := 0
	for _, e := range d.entries {
		for k := uint32(0); k < e.count && i < n; k++ {
			dts[i] = cur
			dur[i] = int64(e.delta)
			cur += int64(e.delta)
			i++
		}
		if i >= n {
			break
		}
	}
	lastDelta := int64(0)
	if len(d.entries) > 0 {
		lastDelta = int64(d.entries[len(d.entries)-1].delta)
	}
	for ; i < n; i++ {
		dts[i] = cur
		dur[i] = lastDelta
		cur += lastDelta
	}
	return dts, dur
}

// sampleMagic prefixes each synthetic sample so recovery can recognise a
// trustworthy media boundary without decoding codecs.
var sampleMagic = []byte("MSMP")
