package audio

import "encoding/binary"

// Header returns the canonical 44-byte PCM WAVE header for format f and
// dataBytes of sample data. The Writer and EncodeWAV share it so the container
// layout cannot drift between streaming and in-memory paths.
func Header(f Format, dataBytes int) []byte {
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+dataBytes))
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], pcmTag)
	binary.LittleEndian.PutUint16(hdr[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(hdr[24:], uint32(f.Rate))
	blockAlign := f.Channels * bitsPerSample / 8
	binary.LittleEndian.PutUint32(hdr[28:], uint32(f.Rate*blockAlign))
	binary.LittleEndian.PutUint16(hdr[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:], bitsPerSample)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(dataBytes))
	return hdr
}
