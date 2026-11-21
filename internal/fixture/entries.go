package fixture

// Minimal sample-entry boxes. The synthetic decoder only checks payload
// CRCs, so codec configuration is present purely for structural validity.

func writeDinf(m *memBuffer) {
	dinf, _ := BeginBox(m, "dinf")
	dref, _ := BeginFullBox(m, "dref", 0, 0)
	writeU32(m, 1)
	url, _ := BeginFullBox(m, "url ", 0, 0x01) // self-contained
	EndBox(m, url)
	EndBox(m, dref)
	EndBox(m, dinf)
}

func writeVideoSampleEntry(m *memBuffer) {
	avc1, _ := BeginBox(m, "avc1")
	var head [78]byte
	putU16(head[6:], 1)              // data reference index
	putU16(head[24:], 320)           // width
	putU16(head[26:], 240)           // height
	putU32(head[28:], 0x00480000)    // h-res 72 dpi
	putU32(head[32:], 0x00480000)    // v-res
	putU16(head[40:], 1)             // frame count
	putU16(head[48:], 16)            // depth
	putU16(head[50:], 0xFFFF)
	m.Write(head[:])
	avcC, _ := BeginBox(m, "avcC")
	m.Write([]byte{1, 0x4D, 0x40, 0x1E, 0xFF, 0xE1, 0, 0, 1, 0x19, 0, 0, 0, 0})
	EndBox(m, avcC)
	EndBox(m, avc1)
}

func writeAudioSampleEntry(m *memBuffer, timescale uint32) {
	mp4a, _ := BeginBox(m, "mp4a")
	var head [28]byte
	putU16(head[6:], 1)            // data reference index
	putU16(head[16:], 2)           // channels
	putU16(head[18:], 16)          // sample size
	putU32(head[24:], timescale << 16)
	m.Write(head[:])
	EndBox(m, mp4a)
}
