package parse

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

func encodeLanguage(lang string) [3]byte {
	if len(lang) != 3 {
		return [3]byte{0x55, 0xC4, 0} // "und"
	}
	a := uint16(lang[0]-0x60) & 0x1F
	b := uint16(lang[1]-0x60) & 0x1F
	c := uint16(lang[2]-0x60) & 0x1F
	packed := (a << 10) | (b << 5) | c
	return [3]byte{byte(packed >> 8), byte(packed), 0}
}

func handlerName(handler string) string {
	switch handler {
	case "vide":
		return "VideoHandler"
	case "soun":
		return "SoundHandler"
	case "text", "sbtl":
		return "TextHandler"
	case "meta":
		return "MetadataHandler"
	default:
		return "Handler"
	}
}
