package samplegen

// TrackSpec describes one generated demo track.
type TrackSpec struct {
	ID            uint32
	Handler       string
	Timescale     uint32
	SampleCount   int
	Delta         uint32
	SampleSize    uint32
	Keyframes     []int
	EditMediaTime int32
	EditDuration  uint32
	IncludeEdit   bool
	ZeroTimescale bool
}

func ftypBox(quickTime bool) *Builder {
	if quickTime {
		return New("ftyp", appendBytes([]byte("qt  "), u32(0), []byte("qt  ")))
	}
	return New("ftyp", appendBytes([]byte("isom"), u32(512), []byte("isomiso2mp41")))
}

func mvhdBox() *Builder {
	body := appendBytes(
		u32(0), u32(0),
		u32(1000), u32(10_000),
		u32(0x00010000),
		u16(0x0100),
		u16(0),
		make([]byte, 10),
		u32(0x00010000), u32(0), u32(0),
		u32(0), u32(0x00010000), u32(0),
		u32(0), u32(0), u32(0x40000000),
		make([]byte, 24),
		u32(3),
	)
	return Full("mvhd", 0, 0, body)
}

func tkhdBox(id uint32, handler string) *Builder {
	body := appendBytes(
		u32(0), u32(0),
		u32(id),
		u32(0),
		u32(6000),
		make([]byte, 8),
		u16(0), u16(0),
		u16(0), u16(0),
		u32(0x00010000), u32(0), u32(0),
		u32(0), u32(0x00010000), u32(0),
		u32(0), u32(0), u32(0x40000000),
	)
	if handler == "soun" {
		body = appendBytes(body, u32(0), u32(0)) // width/height
		body[32] = 0x01                          // tkhd.volume high byte
	} else {
		body = appendBytes(body, u32(320<<16), u32(180<<16))
	}
	return Full("tkhd", 0, 0x000007, body)
}

func mdhdBox(spec TrackSpec) *Builder {
	ts := spec.Timescale
	if spec.ZeroTimescale {
		ts = 0
	}
	duration := uint32(spec.SampleCount) * spec.Delta
	body := appendBytes(
		u32(0), u32(0),
		u32(ts), u32(duration),
		u16(0x55c4), // und language
		u16(0),
	)
	return Full("mdhd", 0, 0, body)
}

func hdlrBox(handler string) *Builder {
	return Full("hdlr", 0, 0, appendBytes(
		u32(0),
		[]byte(handler),
		make([]byte, 12),
		[]byte("Mediascan\000"),
	))
}

func videoSampleEntry() *Builder {
	payload := make([]byte, 78)
	copy(payload[0:6], make([]byte, 6))
	payload[8] = 1 // data reference index
	copy(payload[24:26], u16(320))
	copy(payload[26:28], u16(180))
	copy(payload[28:32], u32(0x00480000))
	copy(payload[32:36], u32(0x00480000))
	copy(payload[40:42], u16(1))
	return New("avc1", payload, New("avcC", []byte{1, 66, 192, 30, 225, 0, 0}))
}

func audioSampleEntry() *Builder {
	payload := make([]byte, 28)
	payload[8] = 1
	copy(payload[16:18], u16(2))
	copy(payload[18:20], u16(16))
	copy(payload[24:28], u32(0xBB800000)) // 48000 in high 16 bits
	return New("mp4a", payload, New("esds", []byte{0, 0, 0, 0}))
}

func stsdBox(handler string) *Builder {
	entry := videoSampleEntry()
	if handler == "soun" {
		entry = audioSampleEntry()
	}
	return Full("stsd", 0, 0, appendBytes(u32(1), entry.serialize(map[string]int{}, 0)))
}

func sttsBox(spec TrackSpec) *Builder {
	return Full("stts", 0, 0, appendBytes(u32(1), u32(uint32(spec.SampleCount)), u32(spec.Delta)))
}

func stscBox() *Builder {
	return Full("stsc", 0, 0, appendBytes(u32(1), u32(1), u32(1), u32(1)))
}

func stszBox(spec TrackSpec) *Builder {
	return Full("stsz", 0, 0, appendBytes(u32(spec.SampleSize), u32(uint32(spec.SampleCount))))
}

func stcoBox(spec TrackSpec, marker string) *Builder {
	body := u32(uint32(spec.SampleCount))
	for i := 0; i < spec.SampleCount; i++ {
		body = append(body, u32(0)...)
	}
	return Full("stco", 0, 0, body).Mark(marker)
}

func stssBox(keyframes []int) *Builder {
	body := u32(uint32(len(keyframes)))
	for _, n := range keyframes {
		body = append(body, u32(uint32(n))...)
	}
	return Full("stss", 0, 0, body)
}

func elstBox(spec TrackSpec) *Builder {
	body := appendBytes(
		u32(1),
		u32(spec.EditDuration),
		mediaTimeToBytes(spec.EditMediaTime),
		u32(0x00010000),
	)
	return Full("elst", 0, 0, body)
}

func mediaTimeToBytes(v int32) []byte {
	return u32(uint32(v))
}

func dinfBox() *Builder {
	url := Full("url ", 0, 0x000001, nil)
	dref := Full("dref", 0, 0, appendBytes(u32(1), url.serialize(map[string]int{}, 0)))
	return New("dinf", nil, dref)
}
