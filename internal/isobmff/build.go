package isobmff

import (
	"encoding/binary"
	"errors"
)

// SampleConfig describes one trun sample to encode.
type SampleConfig struct {
	Duration uint32
	Size     uint32
	Data     []byte
}

// FragmentConfig deterministically builds a small CMAF-style movie fragment.
type FragmentConfig struct {
	Sequence    uint32
	TFDT        uint64
	TFDTVersion byte
	DataOffset  int32
	Samples     []SampleConfig
}

// EncodeBox exports the deterministic box encoder used by fixtures.
func EncodeBox(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

// EncodeFullBox exports the deterministic full-box encoder used by fixtures.
func EncodeFullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(head[0:4], uint32(version)<<24|(flags&0xffffff))
	copy(head[4:], payload)
	return EncodeBox(typ, head)
}

// BuildFragment encodes moof(mfhd,traf(tfdt,trun)) followed by mdat.
func BuildFragment(cfg FragmentConfig) ([]byte, error) {
	if len(cfg.Samples) == 0 {
		return nil, errors.New("fragment requires samples")
	}
	mfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdPayload, cfg.Sequence)
	mfhd := EncodeFullBox("mfhd", 0, 0, mfhdPayload)

	version := cfg.TFDTVersion
	if version == 0 && cfg.TFDT > uint64(^uint32(0)) {
		version = 1
	}
	var tfdtPayload []byte
	if version == 1 {
		tfdtPayload = make([]byte, 8)
		binary.BigEndian.PutUint64(tfdtPayload, cfg.TFDT)
	} else {
		tfdtPayload = make([]byte, 4)
		binary.BigEndian.PutUint32(tfdtPayload, uint32(cfg.TFDT))
	}
	tfdt := EncodeFullBox("tfdt", version, 0, tfdtPayload)

	flags := TrunDataOffsetPresent | TrunSampleDurationPresent | TrunSampleSizePresent
	trunPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(trunPayload, uint32(len(cfg.Samples)))
	offsetField := make([]byte, 4)
	binary.BigEndian.PutUint32(offsetField, uint32(cfg.DataOffset))
	trunPayload = append(trunPayload, offsetField...)

	mdatPayload := make([]byte, 0)
	for i, s := range cfg.Samples {
		size := s.Size
		var data []byte
		if s.Data != nil {
			data = s.Data
			size = uint32(len(data))
		} else if size > 0 {
			data = deterministicSampleBytes(i, size)
		}
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b[0:4], s.Duration)
		binary.BigEndian.PutUint32(b[4:8], size)
		trunPayload = append(trunPayload, b...)
		mdatPayload = append(mdatPayload, data...)
	}
	trun := EncodeFullBox("trun", 0, flags, trunPayload)
	traf := EncodeBox("traf", append(tfdt, trun...))
	moof := EncodeBox("moof", append(mfhd, traf...))

	// mdat immediately follows moof, so its payload starts at len(moof)+8.
	realOffset := int32(len(moof) + 8)
	if cfg.DataOffset != realOffset {
		// Patch the fixed-width data_offset field at count(4) + offset(4).
		binary.BigEndian.PutUint32(trunPayload[4:8], uint32(realOffset))
		trun = EncodeFullBox("trun", 0, flags, trunPayload)
		traf = EncodeBox("traf", append(tfdt, trun...))
		moof = EncodeBox("moof", append(mfhd, traf...))
	}
	mdat := EncodeBox("mdat", mdatPayload)
	return append(moof, mdat...), nil
}

func deterministicSampleBytes(sampleIndex int, size uint32) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte((i*31 + sampleIndex*17) & 0xff)
	}
	return out
}
