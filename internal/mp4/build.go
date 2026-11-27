// Package mp4 serialisation helpers build mfhd, tfdt and trun boxes plus a
// minimal moof+mdat pair. They are used by the fixture builder and tests.
package mp4

// BuildMfhd serialises an mfhd payload with a standard 32-bit sequence number.
func BuildMfhd(seq uint32) []byte {
	p := make([]byte, 8)
	p[0] = 0
	putU32(p[4:8], uint64(seq))
	return Marshal("mfhd", p)
}

// BuildTfdt serialises a v1 tfdt with a 64-bit base decode time.
func BuildTfdt(version uint8, base uint64) []byte {
	if version == 0 {
		p := make([]byte, 8)
		p[0] = 0
		putU32(p[4:8], base)
		return Marshal("tfdt", p)
	}
	p := make([]byte, 12)
	p[0] = 1
	putU64(p[4:12], base)
	return Marshal("tfdt", p)
}

// BuildTrun serialises a trun whose sample entries all carry duration, size,
// flags and composition offset. When flags carries the data-offset bit the
// matching field is written first, mirroring the fixtures the pipeline reads.
func BuildTrun(flags uint32, dataOffset int32, samples []Sample) []byte {
	body := make([]byte, 8, 8+len(samples)*16+4)
	body[0] = 0
	body[1] = byte(flags >> 16)
	body[2] = byte(flags >> 8)
	body[3] = byte(flags)
	putU32(body[4:8], uint64(len(samples)))
	if flags&trunDataOffset != 0 {
		body = append(body, packU32(uint32(dataOffset))...)
	}
	for _, s := range samples {
		if flags&trunSampleDuration != 0 {
			body = append(body, packU32(s.Duration)...)
		}
		if flags&trunSampleSize != 0 {
			body = append(body, packU32(s.Size)...)
		}
		if flags&trunSampleFlags != 0 {
			body = append(body, packU32(s.Flags)...)
		}
		if flags&trunSampleCTO != 0 {
			body = append(body, packU32(uint32(s.CompositionTime))...)
		}
	}
	return Marshal("trun", body)
}

// SampleFlagsFor returns the trun sample-flags word for a key/non-key sample.
func SampleFlagsFor(keyframe bool) uint32 {
	if keyframe {
		return 0x02000000
	}
	return 0x01010000 | sampleIsNonSync
}

// IsKeyframe decodes whether a trun sample-flags word marks a sync sample.
func IsKeyframe(flags uint32) bool { return flags&sampleIsNonSync == 0 }

// Fragment groups the boxes of one movie fragment with their decoded values.
type Fragment struct {
	Mfhd     *Mfhd
	Tfdt     *Tfdt
	Truns    []*Trun
	TrackID  uint32
	Sequence uint32
	BaseTime uint64
}

// Assemble builds a minimal moof+mdat pair with a single run, used to produce
// fixtures and to exercise the parser in tests.
func Assemble(seq uint32, base uint64, flags uint32, samples []Sample, media []byte) []byte {
	const dataOffset = 0
	tfhd := Marshal("tfhd", packTfhd())
	traf := make([]byte, 0, len(tfhd)+12+len(samples)*16+4)
	traf = append(traf, tfhd...)
	traf = append(traf, BuildTfdt(1, base)...)
	traf = append(traf, BuildTrun(flags, dataOffset, samples)...)
	moofBody := make([]byte, 0, 16+len(traf)+12)
	moofBody = append(moofBody, BuildMfhd(seq)...)
	moofBody = append(moofBody, Marshal("traf", traf)...)
	mdat := Marshal("mdat", media)
	return append(Marshal("moof", moofBody), mdat...)
}

func packTfhd() []byte {
	p := make([]byte, 8)
	p[0] = 0
	putU32(p[4:8], 1) // track_ID
	return p
}

func packU32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}
